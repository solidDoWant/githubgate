package server

import (
	"context"
	"errors"
	"strings"

	"github.com/solidDoWant/githubgate/internal/github"
	"github.com/solidDoWant/githubgate/internal/model"
	"github.com/solidDoWant/githubgate/internal/proxy"
)

// AccountLogin names the account with id for the proxy: the owner of the
// first of installations that is on that account, as the App's installation
// listing says. The listing is cached by the App client, so a caller paging
// through a list costs no GitHub call per page.
func (s *Server) AccountLogin(ctx context.Context, installations []*model.Installation, id int64) (string, error) {
	var failure error

	for _, installation := range installations {
		accountID, err := s.installationAccountID(ctx, installation)

		switch {
		case err == nil && accountID == id:
			return installation.Owner, nil
		case err == nil, errors.Is(err, errNoInstallation):
		default:
			var githubError *github.Error
			if errors.As(err, &githubError) && githubError.Condition == github.ConditionInstallationNotFound {
				// The App has no installation on this owner, so it is not
				// on the account either.
				continue
			}

			failure = err
		}
	}

	if failure != nil {
		return "", failure
	}

	return "", proxy.ErrAccountNotFound
}

// errNoInstallation is an installation githubgate has no App or owner for.
var errNoInstallation = errors.New("the installation has no App or owner")

func (s *Server) installationAccountID(ctx context.Context, installation *model.Installation) (int64, error) {
	app := installation.App
	if app == nil || strings.TrimSpace(installation.Owner) == "" {
		return 0, errNoInstallation
	}

	client, err := s.options.GitHub.Client(app.NamespacedName, github.Credentials{
		AppID: app.AppID, PrivateKey: app.PrivateKey(), Version: app.KeyVersion,
	})
	if err != nil {
		return 0, err
	}

	info, err := client.Installation(ctx, installation.Owner)
	if err != nil {
		return 0, err
	}

	return info.AccountID, nil
}
