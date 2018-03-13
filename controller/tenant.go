package controller

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	jwt "github.com/dgrijalva/jwt-go"
	"github.com/fabric8-services/fabric8-tenant/app"
	"github.com/fabric8-services/fabric8-tenant/cluster"
	"github.com/fabric8-services/fabric8-tenant/jsonapi"
	"github.com/fabric8-services/fabric8-tenant/openshift"
	"github.com/fabric8-services/fabric8-tenant/tenant"
	"github.com/fabric8-services/fabric8-tenant/user"
	"github.com/fabric8-services/fabric8-wit/errors"
	"github.com/fabric8-services/fabric8-wit/log"
	"github.com/fabric8-services/fabric8-wit/rest"
	"github.com/goadesign/goa"
	goajwt "github.com/goadesign/goa/middleware/security/jwt"
	uuid "github.com/satori/go.uuid"
	yaml "gopkg.in/yaml.v2"
)

// TenantController implements the status resource.
type TenantController struct {
	*goa.Controller
	tenantService          tenant.Service
	resolveTenant          tenant.Resolve
	userService            user.Service
	resolveCluster         cluster.Resolve
	defaultOpenshiftConfig openshift.Config
	templateVars           map[string]string
}

// NewTenantController creates a status controller.
func NewTenantController(
	service *goa.Service,
	tenantService tenant.Service,
	userService user.Service,
	resolveTenant tenant.Resolve,
	resolveCluster cluster.Resolve,
	defaultOpenshiftConfig openshift.Config,
	templateVars map[string]string) *TenantController {

	return &TenantController{
		Controller:             service.NewController("TenantController"),
		tenantService:          tenantService,
		userService:            userService,
		resolveTenant:          resolveTenant,
		resolveCluster:         resolveCluster,
		defaultOpenshiftConfig: defaultOpenshiftConfig,
		templateVars:           templateVars,
	}
}

// Setup runs the setup action.
func (c *TenantController) Setup(ctx *app.SetupTenantContext) error {
	usrToken := goajwt.ContextJWT(ctx)
	if usrToken == nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError("Missing JWT token"))
	}
	tenantToken := &TenantToken{token: usrToken}
	if c.tenantService.Exists(tenantToken.Subject()) {
		log.Error(ctx, map[string]interface{}{"tenant_id": tenantToken.Subject()}, "a tenant with the same ID already exists")
		return jsonapi.JSONErrorResponse(ctx, errors.NewDataConflictError(fmt.Sprintf("a tenant with the same ID already exists: %s", tenantToken.Subject())))
	}
	// fetch the cluster the user belongs to
	usr, err := c.userService.GetUser(ctx, tenantToken.Subject())
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}

	if usr.Cluster == nil {
		log.Error(ctx, nil, "no cluster defined for tenant")
		return jsonapi.JSONErrorResponse(ctx, errors.NewInternalError(ctx, fmt.Errorf("unable to provision to undefined cluster")))
	}

	// fetch the users cluster token
	openshiftUsername, openshiftUserToken, err := c.resolveTenant(ctx, *usr.Cluster, usrToken.Raw)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":         err,
			"cluster_url": *usr.Cluster,
		}, "unable to fetch tenant token from auth")
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError("Could not resolve user token"))
	}
	log.Debug(ctx, map[string]interface{}{
		"openshift_username": openshiftUsername,
		"openshift_token":    openshiftUserToken},
		"resolved user on cluster")

	// fetch the cluster info
	clustr, err := c.resolveCluster(ctx, *usr.Cluster)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":         err,
			"cluster_url": *usr.Cluster,
		}, "unable to fetch cluster")
		return jsonapi.JSONErrorResponse(ctx, errors.NewInternalError(ctx, err))
	}
	log.Debug(ctx, map[string]interface{}{
		"cluster_api_url": clustr.APIURL,
		"user_id":         tenantToken.Subject().String()},
		"resolved cluster for user")

	// create openshift config
	openshiftConfig := openshift.NewConfig(c.defaultOpenshiftConfig, usr, clustr.User, clustr.Token, clustr.APIURL)
	t := &tenant.Tenant{ID: tenantToken.Subject(), Email: tenantToken.Email()}
	err = c.tenantService.SaveTenant(t)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err": err,
		}, "unable to store tenant configuration")
		return jsonapi.JSONErrorResponse(ctx, errors.NewInternalError(ctx, err))
	}

	// perform the tenant init
	err = openshift.InitTenant(
		ctx,
		openshiftConfig,
		newTenantCallBack(ctx, openshiftConfig.MasterURL, c.tenantService, t),
		openshiftUsername,
		openshiftUserToken,
		c.templateVars)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":     err,
			"os_user": openshiftUsername,
		}, "unable initialize tenant")
		return jsonapi.JSONErrorResponse(ctx, err)
	}

	ctx.ResponseData.Header().Set("Location", rest.AbsoluteURL(ctx.RequestData.Request, app.TenantHref()))
	return ctx.Accepted()
}

// Update runs the setup action.
func (c *TenantController) Update(ctx *app.UpdateTenantContext) error {
	usrToken := goajwt.ContextJWT(ctx)
	if usrToken == nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError("Missing JWT token"))
	}
	tenantToken := &TenantToken{token: usrToken}
	tenant, err := c.tenantService.GetTenant(tenantToken.Subject())
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewNotFoundError("tenants", tenantToken.Subject().String()))
	}

	// fetch the cluster the user belongs to
	usr, err := c.userService.GetUser(ctx, tenantToken.Subject())
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}

	if usr.Cluster == nil {
		log.Error(ctx, nil, "no cluster defined for tenant")
		return jsonapi.JSONErrorResponse(ctx, errors.NewInternalError(ctx, fmt.Errorf("unable to provision to undefined cluster")))
	}

	// fetch the users cluster token
	openshiftUsername, _, err := c.resolveTenant(ctx, *usr.Cluster, usrToken.Raw)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":         err,
			"cluster_url": *usr.Cluster,
		}, "unable to fetch tenant token from auth")
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError("Could not resolve user token"))
	}

	clustr, err := c.resolveCluster(ctx, *usr.Cluster)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":         err,
			"cluster_url": *usr.Cluster,
		}, "unable to fetch cluster")
		return jsonapi.JSONErrorResponse(ctx, errors.NewInternalError(ctx, err))
	}
	log.Info(ctx, map[string]interface{}{"cluster_api_url": clustr.APIURL, "user_id": tenantToken.Subject().String()}, "resolved cluster for user")
	// create openshift config
	openshiftConfig := openshift.NewConfig(c.defaultOpenshiftConfig, usr, clustr.User, clustr.Token, clustr.APIURL)

	go func() {
		ctx := ctx
		t := tenant
		err = openshift.UpdateTenant(
			ctx,
			openshiftConfig,
			newTenantCallBack(ctx, openshiftConfig.MasterURL, c.tenantService, t),
			openshiftUsername,
			c.templateVars)

		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"err":     err,
				"os_user": openshiftUsername,
			}, "unable initialize tenant")
		}
	}()

	ctx.ResponseData.Header().Set("Location", rest.AbsoluteURL(ctx.RequestData.Request, app.TenantHref()))
	return ctx.Accepted()
}

// Clean runs the setup action for the tenant namespaces.
func (c *TenantController) Clean(ctx *app.CleanTenantContext) error {
	usrToken := goajwt.ContextJWT(ctx)
	if usrToken == nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError("Missing JWT token"))
	}
	tenantToken := &TenantToken{token: usrToken}

	// fetch the cluster the user belongs to
	usr, err := c.userService.GetUser(ctx, tenantToken.Subject())
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}

	// restrict deprovision from cluster to internal users only
	removeFromCluster := false
	if usr.FeatureLevel != nil && *usr.FeatureLevel == "internal" {
		removeFromCluster = ctx.Remove
	}

	// fetch the users cluster token
	openshiftUsername, _, err := c.resolveTenant(ctx, *usr.Cluster, usrToken.Raw)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":         err,
			"cluster_url": *usr.Cluster,
		}, "unable to fetch tenant token from auth")
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError("Could not resolve user token"))
	}

	clustr, err := c.resolveCluster(ctx, *usr.Cluster)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"err":         err,
			"cluster_url": *usr.Cluster,
		}, "unable to fetch cluster")
		return jsonapi.JSONErrorResponse(ctx, errors.NewInternalError(ctx, err))
	}

	// create openshift config
	openshiftConfig := openshift.NewConfig(c.defaultOpenshiftConfig, usr, clustr.User, clustr.Token, clustr.APIURL)

	err = openshift.CleanTenant(ctx, openshiftConfig, openshiftUsername, c.templateVars, removeFromCluster)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}
	if removeFromCluster {
		err = c.tenantService.DeleteAll(tenantToken.Subject())
		if err != nil {
			return jsonapi.JSONErrorResponse(ctx, err)
		}
	}
	return ctx.NoContent()
}

// Show runs the setup action.
func (c *TenantController) Show(ctx *app.ShowTenantContext) error {
	token := goajwt.ContextJWT(ctx)
	if token == nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewUnauthorizedError("Missing JWT token"))
	}

	tenantToken := &TenantToken{token: token}
	tenantID := tenantToken.Subject()
	tenant, err := c.tenantService.GetTenant(tenantID)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, errors.NewNotFoundError("tenants", tenantID.String()))
	}

	namespaces, err := c.tenantService.GetNamespaces(tenantID)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, err)
	}

	result := &app.TenantSingle{Data: convertTenant(ctx, tenant, namespaces, c.resolveCluster)}
	return ctx.OK(result)
}

// newTenantCallBack returns a Callback that assumes a new tenant is being created
func newTenantCallBack(ctx context.Context, masterURL string, service tenant.Service, currentTenant *tenant.Tenant) openshift.Callback {
	var maxResourceQuotaStatusCheck int32 = 50 // technically a global retry count across all ResourceQuota on all Tenant Namespaces
	var currentResourceQuotaStatusCheck int32
	return func(statusCode int, method string, request, response map[interface{}]interface{}) (string, map[interface{}]interface{}) {
		log.Debug(ctx, map[string]interface{}{
			"status":    statusCode,
			"method":    method,
			"namespace": openshift.GetNamespace(request),
			"name":      openshift.GetName(request),
			"kind":      openshift.GetKind(request),
			"request":   yamlString(request),
			"response":  yamlString(response),
		}, "resource requested")
		if statusCode == http.StatusConflict {
			if openshift.GetKind(request) == openshift.ValKindNamespace {
				return "", nil
			}
			if openshift.GetKind(request) == openshift.ValKindProjectRequest {
				return "", nil
			}
			if openshift.GetKind(request) == openshift.ValKindPersistenceVolumeClaim {
				return "", nil
			}
			if openshift.GetKind(request) == openshift.ValKindServiceAccount {
				return "", nil
			}
			return "DELETE", request
		} else if statusCode == http.StatusCreated {
			if openshift.GetKind(request) == openshift.ValKindProjectRequest {
				name := openshift.GetName(request)
				service.SaveNamespace(&tenant.Namespace{
					TenantID:  currentTenant.ID,
					Name:      name,
					State:     "created",
					Version:   openshift.GetLabelVersion(request),
					Type:      tenant.GetNamespaceType(name),
					MasterURL: masterURL,
				})

				// HACK to workaround osio applying some dsaas-user permissions async
				// Should loop on a Check if allowed type of call instead
				time.Sleep(time.Second * 5)

			} else if openshift.GetKind(request) == openshift.ValKindNamespace {
				name := openshift.GetName(request)
				service.SaveNamespace(&tenant.Namespace{
					TenantID:  currentTenant.ID,
					Name:      name,
					State:     "created",
					Version:   openshift.GetLabelVersion(request),
					Type:      tenant.GetNamespaceType(name),
					MasterURL: masterURL,
				})
			} else if openshift.GetKind(request) == openshift.ValKindResourceQuota {
				// trigger a check status loop
				time.Sleep(time.Millisecond * 50)
				return "GET", response
			}
			return "", nil
		} else if statusCode == http.StatusOK {
			if method == "DELETE" {
				return "POST", request
			} else if method == "GET" {
				if openshift.GetKind(request) == openshift.ValKindResourceQuota {

					if openshift.HasValidStatus(response) || atomic.LoadInt32(&currentResourceQuotaStatusCheck) >= maxResourceQuotaStatusCheck {
						return "", nil
					}
					atomic.AddInt32(&currentResourceQuotaStatusCheck, 1)
					time.Sleep(time.Millisecond * 50)
					return "GET", response
				}
			}
			return "", nil
		}
		log.Info(ctx, map[string]interface{}{
			"status":    statusCode,
			"method":    method,
			"namespace": openshift.GetNamespace(request),
			"name":      openshift.GetName(request),
			"kind":      openshift.GetKind(request),
			"request":   yamlString(request),
			"response":  yamlString(response),
		}, "unhandled resource response")
		return "", nil
	}
}

func OpenshiftToken(openshiftConfig openshift.Config, token *jwt.Token) (string, error) {
	return "", nil
}

type TenantToken struct {
	token *jwt.Token
}

func (t TenantToken) Subject() uuid.UUID {
	if claims, ok := t.token.Claims.(jwt.MapClaims); ok {
		id, err := uuid.FromString(claims["sub"].(string))
		if err != nil {
			return uuid.UUID{}
		}
		return id
	}
	return uuid.UUID{}
}

func (t TenantToken) Username() string {
	if claims, ok := t.token.Claims.(jwt.MapClaims); ok {
		answer := claims["preferred_username"].(string)
		if len(answer) == 0 {
			answer = claims["username"].(string)
		}
		return answer
	}
	return ""
}

func (t TenantToken) Email() string {
	if claims, ok := t.token.Claims.(jwt.MapClaims); ok {
		return claims["email"].(string)
	}
	return ""
}

func convertTenant(ctx context.Context, tenant *tenant.Tenant, namespaces []*tenant.Namespace, resolveCluster cluster.Resolve) *app.Tenant {
	result := app.Tenant{
		ID:   &tenant.ID,
		Type: "tenants",
		Attributes: &app.TenantAttributes{
			CreatedAt:  &tenant.CreatedAt,
			Email:      &tenant.Email,
			Profile:    &tenant.Profile,
			Namespaces: []*app.NamespaceAttributes{},
		},
	}
	for _, ns := range namespaces {
		c, err := resolveCluster(ctx, ns.MasterURL)
		if err != nil {
			log.Error(ctx, map[string]interface{}{
				"err":         err,
				"cluster_url": ns.MasterURL,
			}, "unable to resolve cluster")
			c = &cluster.Cluster{}
		}
		tenantType := string(ns.Type)
		result.Attributes.Namespaces = append(
			result.Attributes.Namespaces,
			&app.NamespaceAttributes{
				CreatedAt:         &ns.CreatedAt,
				UpdatedAt:         &ns.UpdatedAt,
				ClusterURL:        &ns.MasterURL,
				ClusterAppDomain:  &c.AppDNS,
				ClusterConsoleURL: &c.ConsoleURL,
				ClusterMetricsURL: &c.MetricsURL,
				ClusterLoggingURL: &c.LoggingURL,
				Name:              &ns.Name,
				Type:              &tenantType,
				Version:           &ns.Version,
				State:             &ns.State,
			})
	}
	return &result
}

func yamlString(data map[interface{}]interface{}) string {
	b, err := yaml.Marshal(data)
	if err != nil {
		return fmt.Sprintf("Could not marshal yaml %v", data)
	}
	return string(b)
}
