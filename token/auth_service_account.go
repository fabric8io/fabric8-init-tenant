package token

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/fabric8-services/fabric8-tenant/auth"
	"github.com/fabric8-services/fabric8-tenant/configuration"
	goaclient "github.com/goadesign/goa/client"
	"github.com/pkg/errors"
)

type ServiceAccountTokenService interface {
	Get(ctx context.Context) error
}

type ServiceAccountTokenClient struct {
	Config                  *configuration.Data
	AuthServiceAccountToken string
}

func (c *ServiceAccountTokenClient) Get(ctx context.Context) error {

	authclient, err := CreateClient(c.Config)
	if err != nil {
		return err
	}

	path := auth.ExchangeTokenPath()
	payload := &auth.TokenExchange{
		ClientID: c.Config.GetAuthClientID(),
		ClientSecret: func() *string {
			sec := c.Config.GetClientSecret()
			return &sec
		}(),
		GrantType: c.Config.GetAuthGrantType(),
	}
	contentType := "application/x-www-form-urlencoded"

	res, err := authclient.ExchangeToken(ctx, path, payload, contentType)
	if err != nil {
		return errors.Wrapf(err, "error while doing the request")
	}
	defer res.Body.Close()

	token, err := authclient.DecodeOauthToken(res)
	validationerror := validateError(authclient, res)

	if validationerror != nil {
		return errors.Wrapf(validationerror, "error from server %q", c.Config.GetAuthURL())
	} else if err != nil {
		return errors.Wrapf(err, "error from server %q", c.Config.GetAuthURL())
	}

	if *token.AccessToken != "" {
		c.AuthServiceAccountToken = *token.AccessToken
		return nil
	}

	return fmt.Errorf("received empty token from server %q", c.Config.GetAuthURL())
}

func CreateClient(config *configuration.Data) (*auth.Client, error) {
	u, err := url.Parse(config.GetAuthURL())
	if err != nil {
		return nil, err
	}
	c := auth.New(goaclient.HTTPClientDoer(http.DefaultClient))
	c.Host = u.Host
	c.Scheme = u.Scheme
	return c, nil
}