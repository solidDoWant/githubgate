// Package app wires the githubgate server: flags, the controller-runtime
// manager with leader election, the token API and health listeners, the
// optional admission webhook and the shutdown sequence.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	githubgatev1alpha1 "github.com/solidDoWant/githubgate/api/v1alpha1"
	"github.com/solidDoWant/githubgate/internal/auth"
	"github.com/solidDoWant/githubgate/internal/controller"
	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
	"github.com/solidDoWant/githubgate/internal/server"
	"github.com/solidDoWant/githubgate/internal/webhook"
)

const (
	defaultAudience      = "githubgate"
	leaderElectionID     = "githubgate.githubgate.soliddowant.dev"
	webhookPath          = "/validate"
	webhookPort          = 9443
	defaultProxyAddress  = ":9443"
	defaultShutdownDelay = 5 * time.Second
)

// Config is the server's configuration.
type Config struct {
	AdminNamespaces []string
	Audience        string
	Webhook         bool
	WebhookCertDir  string
	WebhookPort     int
	CertFile        string
	KeyFile         string
	LeaderElect     bool
	ShutdownDelay   time.Duration
	APIAddress      string
	HealthAddress   string
	GitHubBaseURL   string
	// Proxy turns on proxy mode's listener on ProxyAddress, served with
	// the certificate in ProxyCertFile and ProxyKeyFile.
	Proxy         bool
	ProxyAddress  string
	ProxyCertFile string
	ProxyKeyFile  string
	// Namespace is the server's own namespace: the default admin namespace
	// and where the leader lease lives.
	Namespace string

	// RestConfig is the cluster connection; the in-cluster or kubeconfig one when nil.
	RestConfig *rest.Config
	// API, Health and ProxyListener replace the listeners opened on the
	// addresses when set.
	API, Health, ProxyListener net.Listener
	// Gatherer is where metrics are registered and what /metrics serves; a
	// fresh registry when nil.
	Gatherer Registry
}

// Registry registers collectors and gathers them.
type Registry interface {
	prometheus.Gatherer
	prometheus.Registerer
}

// ParseFlags reads the configuration from command-line arguments and the
// POD_NAMESPACE environment variable.
func ParseFlags(args []string, getenv func(string) string) (Config, error) {
	config := Config{Namespace: getenv("POD_NAMESPACE")}

	var admin string

	flags := flag.NewFlagSet("githubgate", flag.ContinueOnError)
	flags.StringVar(&admin, "admin-namespaces", "", "comma-separated namespaces whose GitHubApps and GitHubInstallations are used (default: the server's own namespace)")
	flags.StringVar(&config.Audience, "audience", defaultAudience, "audience of the ServiceAccount tokens callers send")
	flags.BoolVar(&config.Webhook, "webhook", false, "serve the validating admission webhook")
	flags.StringVar(&config.WebhookCertDir, "webhook-cert-dir", "", "directory with tls.crt and tls.key for the webhook")
	flags.IntVar(&config.WebhookPort, "webhook-port", webhookPort, "port of the webhook")
	flags.StringVar(&config.CertFile, "tls-cert-file", "", "certificate of the token API")
	flags.StringVar(&config.KeyFile, "tls-key-file", "", "private key of the token API")
	flags.BoolVar(&config.LeaderElect, "leader-elect", false, "elect a leader for the status controller")
	flags.DurationVar(&config.ShutdownDelay, "shutdown-delay", defaultShutdownDelay, "how long to keep serving after SIGTERM while /readyz fails")
	flags.StringVar(&config.APIAddress, "api-address", ":8443", "address of the token API")
	flags.StringVar(&config.HealthAddress, "health-address", ":8080", "address of /healthz, /readyz and /metrics")
	flags.BoolVar(&config.Proxy, "proxy", false, "serve proxy mode")
	flags.StringVar(&config.ProxyAddress, "proxy-address", defaultProxyAddress, "address of the proxy")
	flags.StringVar(&config.ProxyCertFile, "proxy-tls-cert-file", "", "certificate of the proxy, for api.github.com, uploads.github.com and github.com")
	flags.StringVar(&config.ProxyKeyFile, "proxy-tls-key-file", "", "private key of the proxy")
	flags.StringVar(&config.GitHubBaseURL, "github-base-url", "", "GitHub REST API root (default api.github.com)")

	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}

	for _, namespace := range strings.Split(admin, ",") {
		if namespace = strings.TrimSpace(namespace); namespace != "" {
			config.AdminNamespaces = append(config.AdminNamespaces, namespace)
		}
	}

	if len(config.AdminNamespaces) == 0 && config.Namespace != "" {
		config.AdminNamespaces = []string{config.Namespace}
	}

	return config, config.validate()
}

func (c Config) validate() error {
	switch {
	case len(c.AdminNamespaces) == 0:
		return errors.New("no admin namespaces: set --admin-namespaces or POD_NAMESPACE")
	case c.CertFile == "" || c.KeyFile == "":
		return errors.New("--tls-cert-file and --tls-key-file are required")
	case c.Webhook && c.WebhookCertDir == "":
		return errors.New("--webhook needs --webhook-cert-dir")
	case c.LeaderElect && c.Namespace == "":
		return errors.New("--leader-elect needs POD_NAMESPACE")
	case c.Proxy && (c.ProxyCertFile == "" || c.ProxyKeyFile == ""):
		return errors.New("--proxy needs --proxy-tls-cert-file and --proxy-tls-key-file")
	case c.Proxy && c.Webhook && c.ProxyListener == nil && samePort(c.ProxyAddress, c.WebhookPort):
		return errors.New("--proxy-address and --webhook-port use the same port")
	}

	return nil
}

// Run starts everything and blocks until ctx ends and the shutdown is done.
func Run(ctx context.Context, config Config) error {
	if err := config.validate(); err != nil {
		return err
	}

	// Fail before touching the cluster when the TLS files are unusable.
	if _, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile); err != nil {
		return fmt.Errorf("loading the TLS files %s and %s: %w", config.CertFile, config.KeyFile, err)
	}

	if config.Proxy {
		if _, err := tls.LoadX509KeyPair(config.ProxyCertFile, config.ProxyKeyFile); err != nil {
			return fmt.Errorf("loading the proxy's TLS files %s and %s: %w", config.ProxyCertFile, config.ProxyKeyFile, err)
		}
	}

	restConfig := config.RestConfig
	if restConfig == nil {
		var err error
		if restConfig, err = ctrl.GetConfig(); err != nil {
			return fmt.Errorf("connecting to the cluster: %w", err)
		}
	}

	scheme := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(scheme), githubgatev1alpha1.AddToScheme(scheme)); err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	log := logr.FromSlogHandler(logger.Handler())
	ctrl.SetLogger(log)

	managerOptions := ctrl.Options{
		Scheme:                        scheme,
		Cache:                         model.CacheOptions(config.AdminNamespaces),
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                config.LeaderElect,
		LeaderElectionID:              leaderElectionID,
		LeaderElectionNamespace:       config.Namespace,
		LeaderElectionReleaseOnCancel: true,
	}
	managerOptions.Cache.Scheme = scheme

	if config.Webhook {
		managerOptions.WebhookServer = crwebhook.NewServer(crwebhook.Options{
			Port: config.WebhookPort, CertDir: config.WebhookCertDir,
		})
	}

	manager, err := ctrl.NewManager(restConfig, managerOptions)
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}

	gatherer := config.Gatherer
	if gatherer == nil {
		gatherer = prometheus.NewRegistry()
	}

	githubMetrics := github.NewMetrics()
	serverMetrics := server.NewMetrics()
	gatherer.MustRegister(append(githubMetrics.Collectors(), serverMetrics.Collectors()...)...)

	githubRegistry := github.NewRegistry(github.Options{BaseURL: config.GitHubBaseURL, Metrics: githubMetrics})

	store := &model.Store{}
	watcher := &model.Watcher{Cache: manager.GetCache(), Store: store, AdminNamespaces: config.AdminNamespaces, Log: log}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	authenticator, err := auth.NewTokenReviewAuthenticator(clientset.AuthenticationV1().TokenReviews(), config.Audience)
	if err != nil {
		return err
	}

	if err := errors.Join(
		manager.Add(watcher),
		controller.Setup(manager, controller.Options{AdminNamespaces: config.AdminNamespaces, GitHub: githubRegistry}),
	); err != nil {
		return err
	}

	if config.Webhook {
		handler := webhook.New(config.AdminNamespaces, installationReader{manager.GetClient()})
		manager.GetWebhookServer().Register(webhookPath, handler)
	}

	api, health, err := listeners(config)
	if err != nil {
		return err
	}

	srv := server.New(server.Options{
		Authenticator: authenticator,
		Store:         store,
		GitHub:        githubRegistry,
		Metrics:       serverMetrics,
		Audit:         slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	})

	runOptions := server.RunOptions{
		API: api, Health: health, CertFile: config.CertFile, KeyFile: config.KeyFile,
		ShutdownDelay: config.ShutdownDelay, Gatherer: gatherer,
	}

	if config.Proxy {
		if runOptions.Proxy, err = proxyListener(config); err != nil {
			return errors.Join(err, api.Close(), health.Close())
		}

		runOptions.ProxyHandler = proxy.NewHandler(proxy.Options{Authenticator: authenticator, Ready: store.Ready})
		runOptions.ProxyTLS = proxy.TLSConfig
		runOptions.ProxyCertFile, runOptions.ProxyKeyFile = config.ProxyCertFile, config.ProxyKeyFile
	}

	// The manager outlives the servers' shutdown delay, so the snapshot
	// keeps updating while /readyz already fails.
	managerCtx, stopManager := context.WithCancel(context.WithoutCancel(ctx))
	defer stopManager()

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		defer stopManager()

		return srv.Run(groupCtx, runOptions)
	})
	group.Go(func() error {
		err := manager.Start(managerCtx)
		if err != nil {
			stopManager()
		}

		return err
	})

	return group.Wait()
}

func listeners(config Config) (api, health net.Listener, err error) {
	api, health = config.API, config.Health

	if api == nil {
		if api, err = net.Listen("tcp", config.APIAddress); err != nil {
			return nil, nil, fmt.Errorf("listening on %s: %w", config.APIAddress, err)
		}
	}

	if health == nil {
		if health, err = net.Listen("tcp", config.HealthAddress); err != nil {
			_ = api.Close()

			return nil, nil, fmt.Errorf("listening on %s: %w", config.HealthAddress, err)
		}
	}

	return api, health, nil
}

func proxyListener(config Config) (net.Listener, error) {
	if config.ProxyListener != nil {
		return config.ProxyListener, nil
	}

	listener, err := net.Listen("tcp", config.ProxyAddress)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", config.ProxyAddress, err)
	}

	return listener, nil
}

// samePort reports whether address listens on port.
func samePort(address string, port int) bool {
	_, value, err := net.SplitHostPort(address)

	return err == nil && value == strconv.Itoa(port)
}

// installationReader gives the webhook the installations the manager's cache holds.
type installationReader struct{ reader client.Reader }

func (r installationReader) GetInstallation(ctx context.Context, name types.NamespacedName) (*githubgatev1alpha1.GitHubInstallation, error) {
	var installation githubgatev1alpha1.GitHubInstallation
	if err := r.reader.Get(ctx, name, &installation); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}

		return nil, err
	}

	return &installation, nil
}
