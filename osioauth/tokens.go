package osioauth

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"net/url"

	"github.com/fabric8-services/fabric8-tenant/configuration"
	"github.com/pkg/errors"
)

type authAccessToken struct {
	AccessToken string `json:"access_token,omitempty"`
}

type authEerror struct {
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
	Status string `json:"status,omitempty"`
	Title  string `json:"title,omitempty"`
}

type errorResponse struct {
	Errors []authEerror `json:"errors,omitempty"`
}

func GetAuthAccessToken() (string, error) {
	config, err := configuration.GetData()
	if err != nil {
		return "", errors.Wrapf(err, "failed to setup the configuration")
	}

	payload := strings.NewReader("grant_type=" + config.GetAuthGrantType() + "&client_id=" +
		config.GetAuthClientID() + "&client_secret=" + config.GetClientSecret())

	req, err := http.NewRequest("POST", config.GetAuthURL()+"/api/token", payload)
	if err != nil {
		return "", errors.Wrapf(err, "error creating request object")
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.Wrapf(err, "error while doing the request")
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", errors.Wrapf(err, "error reading response")
	}

	if res.StatusCode != 200 {
		var e errorResponse
		json.Unmarshal(body, &e)

		var output string
		for _, error := range e.Errors {
			output += fmt.Sprintf("%s: %s %s, %s\n", error.Title, error.Status, error.Code, error.Detail)
		}
		return "", fmt.Errorf("error from server %s: %s", config.GetAuthURL(), output)
	}

	var response authAccessToken
	err = json.Unmarshal(body, &response)
	if err != nil {
		return "", errors.Wrapf(err, "error unmarshalling the response")
	}

	//os.Setenv("F8_AUTH_GRANT_TYPE", "barfooo")
	return strings.TrimSpace(response.AccessToken), nil
}

func GetOpenShiftToken(cluster string) (string, error) {

	config, err := configuration.GetData()
	if err != nil {
		return "", errors.Wrapf(err, "failed to setup the configuration")
	}

	token, err := GetAuthAccessToken()
	if err != nil {
		return "", errors.Wrapf(err, "failed to get access token")
	}
	if token == "" {
		return "", fmt.Errorf("failed to get access token")
	}

	// a normal query will look like following
	// http://auth-fabric8.192.168.42.181.nip.io/api/token?for=https://api.starter-us-east-2a.openshift.com
	u, err := url.Parse(config.GetAuthURL())
	if err != nil {
		return "", errors.Wrapf(err, "error parsing auth url")
	}
	u.Path = "/api/token"
	q := u.Query()
	q.Set("for", cluster)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return "", errors.Wrapf(err, "error creating request object")
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Authorization", "Bearer "+token)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.Wrapf(err, "error while doing the request")
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", errors.Wrapf(err, "error reading response")
	}

	var response authAccessToken
	err = json.Unmarshal(body, &response)
	if err != nil {
		return "", errors.Wrapf(err, "error unmarshalling the response")
	}

	return strings.TrimSpace(response.AccessToken), nil
}
