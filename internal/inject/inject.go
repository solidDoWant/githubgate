// Package inject serves the mutating admission webhook that adds the
// interceptor sidecar to pods in opted-in namespaces.
package inject

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/solidDoWant/githubgate/internal/intercept"
)

const (
	// InjectAnnotation set to "false" opts a pod out.
	InjectAnnotation = "githubgate.soliddowant.dev/inject"
	// InjectedAnnotation marks a pod the webhook has mutated.
	InjectedAnnotation = "githubgate.soliddowant.dev/injected"

	// ListenAnnotation on a pod sets the interceptor's listen address,
	// "<IP>:443", in place of DefaultListen.
	ListenAnnotation = "githubgate.soliddowant.dev/listen"
	// DefaultListen is the interceptor's own loopback address.
	DefaultListen = "127.43.0.1:443"

	// SidecarName is the interceptor's container name.
	SidecarName = "githubgate-intercept"

	tokenVolume = "githubgate-token"
	trustVolume = "githubgate-trust"
	stateVolume = "githubgate-intercept-state"
	// extraCAVolume holds the namespace's extra CA ConfigMap, mounted only in the interceptor.
	extraCAVolume = "githubgate-extra-ca"

	tokenDir = "/var/run/secrets/githubgate"
	trustDir = "/var/run/githubgate/trust"
	stateDir = "/var/run/githubgate/state"
	joinRoot = "/var/run/githubgate/join"
	extraDir = "/var/run/githubgate/extra-ca"

	// ExtraCAConfigMapAnnotation on a namespace names a ConfigMap whose ca.crt joins every bundle in the namespace's pods.
	ExtraCAConfigMapAnnotation = "githubgate.soliddowant.dev/extra-ca-configmap"
	extraCAKey                 = "ca.crt"

	unprivilegedPortSysctl = "net.ipv4.ip_unprivileged_port_start"
	privilegedPortLimit    = 443

	notConfigured = "githubgate: injection isn't configured (proxy CA)"
	rejectPrefix  = "githubgate: pod can't be intercepted: "

	envProxyAddress = "GITHUBGATE_INTERCEPT_PROXY_ADDRESS"
	envProxyCA      = "GITHUBGATE_INTERCEPT_PROXY_CA"
	envExtraCA      = "GITHUBGATE_INTERCEPT_EXTRA_CA"
)

var (
	githubHosts   = []string{"api.github.com", "uploads.github.com", "github.com"}
	reservedNames = []string{tokenVolume, trustVolume, stateVolume, extraCAVolume}
	proxyVars     = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}
	noProxyVars   = []string{"NO_PROXY", "no_proxy"}
	// trustVars are the trust variables and the file each points at.
	trustVars = []struct{ name, file string }{
		{"SSL_CERT_FILE", "bundle.crt"},
		{"CURL_CA_BUNDLE", "bundle.crt"},
		{"GIT_SSL_CAINFO", "bundle.crt"},
		{"REQUESTS_CA_BUNDLE", "bundle.crt"},
		{"NIX_SSL_CERT_FILE", "bundle.crt"},
		{"NODE_EXTRA_CA_CERTS", "ca.crt"},
	}
)

// Config configures the webhook.
type Config struct {
	// Image is the digest-pinned client image.
	Image string
	// ProxyAddress is the proxy Service's host:port.
	ProxyAddress string
	// Audience is the ServiceAccount token audience.
	Audience string
	// ProxyCAFile holds the proxy's CA certificate; it is read on every
	// request, so a rotation applies to new pods without a restart.
	ProxyCAFile string
	// ExtraCAFile is an optional file of extra CA certificates.
	ExtraCAFile string
	// Cluster reads the namespace and its extra CA ConfigMap. Nil means no
	// namespace annotation is honoured.
	Cluster Cluster
}

// Cluster is what the webhook reads from the API server.
type Cluster interface {
	// NamespaceAnnotations returns the annotations of the namespace.
	NamespaceAnnotations(ctx context.Context, namespace string) (map[string]string, error)
	// ConfigMapHasKey reports whether the ConfigMap exists and holds the key.
	ConfigMapHasKey(ctx context.Context, namespace, name, key string) (bool, error)
}

// Handler is the mutating webhook.
type Handler struct{ config Config }

// New returns a handler for config.
func New(config Config) *Handler { return &Handler{config: config} }

// readCertificates returns the PEM in the file, or an error when it holds no certificate.
func readCertificates(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	var found bool

	for rest := data; ; {
		var block *pem.Block

		if block, rest = pem.Decode(rest); block == nil {
			break
		}

		if block.Type == "CERTIFICATE" {
			if _, err := x509.ParseCertificate(block.Bytes); err == nil {
				found = true
			}
		}
	}

	if !found {
		return "", errors.New("no certificate")
	}

	return string(data), nil
}

// Ready reports whether the proxy CA file holds a certificate.
func (h *Handler) Ready() bool {
	_, err := readCertificates(h.config.ProxyCAFile)

	return err == nil
}

// ServeHTTP serves the handler on its own, for callers without a webhook server.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	(&admission.Webhook{Handler: h}).ServeHTTP(writer, request)
}

// Handle decides on one pod CREATE.
func (h *Handler) Handle(ctx context.Context, request admission.Request) admission.Response {
	proxyCA, err := readCertificates(h.config.ProxyCAFile)
	if err != nil {
		return admission.Denied(notConfigured)
	}

	extraCA := ""

	if h.config.ExtraCAFile != "" {
		if extraCA, err = readCertificates(h.config.ExtraCAFile); err != nil {
			return admission.Denied("githubgate: injection isn't configured (extra CA)")
		}
	}

	var pod corev1.Pod
	if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
		return admission.Denied("githubgate: can't read the pod")
	}

	if pod.Annotations[InjectAnnotation] == "false" {
		return admission.Allowed("githubgate: pod opted out")
	}

	namespace := request.Namespace
	if namespace == "" {
		namespace = pod.Namespace
	}

	extraConfigMap, denial := h.namespaceExtraCA(ctx, namespace)
	if denial != "" {
		return admission.Denied(denial)
	}

	listen, reason := listenAddress(&pod)
	if reason != "" {
		return admission.Denied(rejectPrefix + reason)
	}

	injected := pod.Annotations[InjectedAnnotation] == "true"

	var prior *corev1.Container

	if injected {
		// Drop what an earlier run added, so the checks see only what
		// others set and the changes below rebuild it.
		prior = findSidecar(&pod)
		removeOwn(&pod, listen)
	}

	if reason := reject(&pod, injected); reason != "" {
		return admission.Denied(rejectPrefix + reason)
	}

	joins, reason := planJoins(&pod, prior)
	if reason != "" {
		return admission.Denied(rejectPrefix + reason)
	}

	mutate(&pod, h.config, listen, proxyCA, extraCA, extraConfigMap, joins)

	mutated, err := json.Marshal(&pod)
	if err != nil {
		return admission.Denied("githubgate: can't write the pod")
	}

	return admission.PatchResponseFromRaw(request.Object.Raw, mutated)
}

// namespaceExtraCA returns the ConfigMap the namespace names as its extra CA,
// or a denial message when the namespace can't be read or the ConfigMap is
// missing.
func (h *Handler) namespaceExtraCA(ctx context.Context, namespace string) (name, denial string) {
	if h.config.Cluster == nil {
		return "", ""
	}

	annotations, err := h.config.Cluster.NamespaceAnnotations(ctx, namespace)
	if err != nil {
		return "", "githubgate: can't read the namespace"
	}

	name = annotations[ExtraCAConfigMapAnnotation]
	if name == "" {
		return "", ""
	}

	found, err := h.config.Cluster.ConfigMapHasKey(ctx, namespace, name, extraCAKey)
	if err != nil {
		return "", "githubgate: can't read ConfigMap " + name
	}

	if !found {
		return "", fmt.Sprintf("%sConfigMap %q named by the %s annotation of the namespace is missing or has no %s", rejectPrefix, name, ExtraCAConfigMapAnnotation, extraCAKey)
	}

	return name, ""
}

// listenAddress returns the interceptor's listen address from the pod's
// annotation, or the default, or why the annotation is refused: it must be
// a specific IP (anything that reaches an unspecified one could make
// requests as the pod's ServiceAccount) with port 443.
func listenAddress(pod *corev1.Pod) (netip.AddrPort, string) {
	value, set := pod.Annotations[ListenAnnotation]
	if !set {
		return netip.MustParseAddrPort(DefaultListen), ""
	}

	addrPort, err := intercept.ListenAddress(value)
	if err != nil || addrPort.Port() != privilegedPortLimit {
		return netip.AddrPort{}, fmt.Sprintf("the %s annotation %q isn't a specific IP with port 443", ListenAnnotation, value)
	}

	return addrPort, ""
}

func ownAlias(listen netip.AddrPort) func(corev1.HostAlias) bool {
	return func(alias corev1.HostAlias) bool {
		return alias.IP == listen.Addr().String() && slices.Equal(alias.Hostnames, githubHosts)
	}
}

// ownTrustValue reports whether name=value is one the webhook sets.
func ownTrustValue(name, value string) bool {
	for _, variable := range trustVars {
		if variable.name == name {
			return value == trustDir+"/"+variable.file
		}
	}

	return name == "GH_TOKEN" && value == "githubgate"
}

// ownTrustMount reports whether mount is the trust mount the webhook adds to
// a workload container. Another mount of the trust volume, such as the one
// KubeVirt adds to share it with a VM guest over virtiofs, belongs to whoever
// added it and is kept.
func ownTrustMount(mount corev1.VolumeMount) bool {
	return mount.Name == trustVolume && mount.MountPath == trustDir && mount.SubPath == "" && mount.SubPathExpr == ""
}

// removeOwn removes the trust variables and mounts the webhook added, and
// the host alias, so a reinvocation starts from what others left. The
// sidecar and its volumes stay for the sidecar-presence check; mutate
// replaces them.
func removeOwn(pod *corev1.Pod, listen netip.AddrPort) {
	pod.Spec.HostAliases = slices.DeleteFunc(pod.Spec.HostAliases, ownAlias(listen))

	strip := func(container *corev1.Container) {
		container.Env = slices.DeleteFunc(container.Env, func(env corev1.EnvVar) bool {
			return env.ValueFrom == nil && ownTrustValue(env.Name, env.Value)
		})
		if container.Name != SidecarName {
			container.VolumeMounts = slices.DeleteFunc(container.VolumeMounts, ownTrustMount)
		}
	}

	for index := range pod.Spec.InitContainers {
		strip(&pod.Spec.InitContainers[index])
	}

	for index := range pod.Spec.Containers {
		strip(&pod.Spec.Containers[index])
	}
}

func hasEnv(container *corev1.Container, names ...string) bool {
	for _, env := range container.Env {
		if slices.Contains(names, env.Name) {
			return true
		}
	}

	return false
}

// reject returns why the pod can't be intercepted, or "".
func reject(pod *corev1.Pod, injected bool) string {
	spec := &pod.Spec

	if spec.HostNetwork {
		return "hostNetwork is true"
	}

	for _, alias := range spec.HostAliases {
		for _, hostname := range alias.Hostnames {
			if slices.Contains(githubHosts, strings.ToLower(strings.TrimSuffix(hostname, "."))) {
				return "hostAliases names " + hostname
			}
		}
	}

	for _, sysctl := range podSysctls(spec) {
		if sysctl.Name != unprivilegedPortSysctl {
			continue
		}

		value, err := strconv.Atoi(sysctl.Value)
		if err != nil || value > privilegedPortLimit {
			return unprivilegedPortSysctl + " is above 443"
		}
	}

	sidecars := 0

	for _, group := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for index := range group {
			container := &group[index]

			if container.Name == SidecarName {
				sidecars++

				if !injected {
					return "a container is named " + SidecarName
				}

				continue
			}

			for _, port := range container.Ports {
				if port.ContainerPort == 443 {
					return "container " + container.Name + " uses port 443"
				}
			}

			for _, mount := range container.VolumeMounts {
				if mount.MountPath == trustDir && !ownTrustMount(mount) {
					return "container " + container.Name + " has a mount at " + trustDir + " that githubgate didn't add"
				}
			}

			if !injected {
				for _, mount := range container.VolumeMounts {
					if slices.Contains(reservedNames, mount.Name) {
						return "a mount is named " + mount.Name
					}
				}
			}
		}
	}

	if !injected {
		for _, volume := range spec.Volumes {
			if slices.Contains(reservedNames, volume.Name) {
				return "a volume is named " + volume.Name
			}
		}
	}

	if injected && (len(spec.InitContainers) == 0 || !hasSidecar(spec.InitContainers) || sidecars != 1) {
		return "the injected annotation is set without the " + SidecarName + " init container"
	}

	return ""
}

func podSysctls(spec *corev1.PodSpec) []corev1.Sysctl {
	if spec.SecurityContext == nil {
		return nil
	}

	return spec.SecurityContext.Sysctls
}

func hasSidecar(containers []corev1.Container) bool {
	return slices.ContainsFunc(containers, func(container corev1.Container) bool { return container.Name == SidecarName })
}

func mutate(pod *corev1.Pod, config Config, listen netip.AddrPort, proxyCA, extraCA, extraConfigMap string, joins []join) {
	spec := &pod.Spec

	spec.HostAliases = append(spec.HostAliases, corev1.HostAlias{IP: listen.Addr().String(), Hostnames: slices.Clone(githubHosts)})

	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}

	if !slices.ContainsFunc(spec.SecurityContext.Sysctls, func(sysctl corev1.Sysctl) bool { return sysctl.Name == unprivilegedPortSysctl }) {
		spec.SecurityContext.Sysctls = append(spec.SecurityContext.Sysctls, corev1.Sysctl{Name: unprivilegedPortSysctl, Value: "443"})
	}

	// The sidecar comes out and goes back first, so every other container is after it.
	spec.InitContainers = slices.DeleteFunc(spec.InitContainers, func(container corev1.Container) bool { return container.Name == SidecarName })

	for index := range spec.InitContainers {
		prepare(&spec.InitContainers[index], joins)
	}

	for index := range spec.Containers {
		prepare(&spec.Containers[index], joins)
	}

	spec.InitContainers = append([]corev1.Container{sidecar(config, listen, proxyCA, extraCA, extraConfigMap, joins)}, spec.InitContainers...)

	spec.Volumes = slices.DeleteFunc(spec.Volumes, func(volume corev1.Volume) bool { return slices.Contains(reservedNames, volume.Name) })
	spec.Volumes = append(spec.Volumes, volumes(config, extraConfigMap)...)

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[InjectedAnnotation] = "true"
}

// prepare adds the no-proxy hosts, the trust variables and the trust mount.
func prepare(container *corev1.Container, joins []join) {
	if hasEnv(container, proxyVars...) || len(container.EnvFrom) > 0 {
		for _, name := range noProxyVars {
			addNoProxy(container, name)
		}
	}

	for _, variable := range trustVars {
		if joined := joinFor(joins, container.Name, variable.name); joined != nil {
			for index := range container.Env {
				if container.Env[index].Name == variable.name {
					container.Env[index].Value = joined.trustPath()
				}
			}

			continue
		}

		container.Env = append(container.Env, corev1.EnvVar{Name: variable.name, Value: trustDir + "/" + variable.file})
	}

	if !hasEnv(container, "GH_TOKEN", "GITHUB_TOKEN") {
		container.Env = append(container.Env, corev1.EnvVar{Name: "GH_TOKEN", Value: "githubgate"})
	}

	container.VolumeMounts = slices.DeleteFunc(container.VolumeMounts, ownTrustMount)
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: trustVolume, MountPath: trustDir, ReadOnly: true})
}

// addNoProxy makes the variable name list every GitHub host. A variable
// that comes from a reference can't be extended and is left alone.
func addNoProxy(container *corev1.Container, name string) {
	for index := range container.Env {
		env := &container.Env[index]
		if env.Name != name {
			continue
		}

		if env.ValueFrom != nil {
			return
		}

		listed := strings.Split(env.Value, ",")
		for _, host := range githubHosts {
			if !slices.Contains(listed, host) {
				if env.Value != "" {
					env.Value += ","
				}

				env.Value += host
			}
		}

		return
	}

	value := strings.Join(githubHosts, ",")
	if len(container.EnvFrom) > 0 {
		// Kubelet resolves envFrom before env and expands $(NO_PROXY) from it, so
		// a value from a ConfigMap or Secret is kept without reading it. Undefined,
		// the literal stays as a pattern that matches no host.
		value = "$(NO_PROXY)," + value
	}

	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
}

func volumes(config Config, extraConfigMap string) []corev1.Volume {
	audience, expiry := config.Audience, int64(3600)
	state, trust := resource.MustParse("1Mi"), resource.MustParse("4Mi")

	all := []corev1.Volume{
		{Name: tokenVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: audience, ExpirationSeconds: &expiry, Path: "token",
			}}},
		}}},
		{Name: stateVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &state}}},
		{Name: trustVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &trust}}},
	}

	if extraConfigMap != "" {
		all = append(all, corev1.Volume{Name: extraCAVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: extraConfigMap},
			Items:                []corev1.KeyToPath{{Key: extraCAKey, Path: extraCAKey}},
		}}})
	}

	return all
}

func sidecar(config Config, listen netip.AddrPort, proxyCA, extraCA, extraConfigMap string, joins []join) corev1.Container {
	env := []corev1.EnvVar{
		{Name: envProxyAddress, Value: config.ProxyAddress},
		{Name: envProxyCA, Value: proxyCA},
	}

	if extraCA != "" {
		env = append(env, corev1.EnvVar{Name: envExtraCA, Value: extraCA})
	}

	args := []string{"intercept"}
	probe := []string{"/githubgate-client", "intercept", "--probe"}

	// The interceptor's own default is DefaultListen; only another address is passed.
	if listen.String() != DefaultListen {
		args = append(args, "--listen", listen.String())
		probe = append(probe, "--listen", listen.String())
	}

	mounts := []corev1.VolumeMount{
		{Name: tokenVolume, MountPath: tokenDir, ReadOnly: true},
		{Name: stateVolume, MountPath: stateDir},
		{Name: trustVolume, MountPath: trustDir},
	}

	if extraConfigMap != "" {
		args = append(args, "--extra-ca", "@"+extraDir+"/"+extraCAKey)
		mounts = append(mounts, corev1.VolumeMount{Name: extraCAVolume, MountPath: extraDir, ReadOnly: true})
	}

	for _, joined := range joins {
		args = append(args, "--join", joined.container+"/"+joined.variable+"="+joined.source)
		mounts = append(mounts, joined.mount)
	}

	always := corev1.ContainerRestartPolicyAlways
	user := int64(65532)
	no, yes := false, true

	return corev1.Container{
		Name:          SidecarName,
		Image:         config.Image,
		Args:          args,
		Env:           env,
		RestartPolicy: &always,
		StartupProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: probe}},
			PeriodSeconds:    1,
			TimeoutSeconds:   10,
			FailureThreshold: 30,
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: &user, RunAsGroup: &user, RunAsNonRoot: &yes,
			AllowPrivilegeEscalation: &no,
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			ReadOnlyRootFilesystem:   &yes,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi"), corev1.ResourceCPU: resource.MustParse("10m")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		VolumeMounts: mounts,
	}
}

func findSidecar(pod *corev1.Pod) *corev1.Container {
	for index := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[index].Name == SidecarName {
			return &pod.Spec.InitContainers[index]
		}
	}

	return nil
}
