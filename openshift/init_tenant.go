package openshift

import (
	"context"
	"reflect"
	"sync"

	"github.com/fabric8-services/fabric8-tenant/errors"
	"github.com/fabric8-services/fabric8-tenant/tenant"
	"github.com/fabric8-services/fabric8-wit/log"
	errs "github.com/pkg/errors"
	yaml "gopkg.in/yaml.v2"
)

const (
	varProjectName           = "PROJECT_NAME"
	varProjectTemplateName   = "PROJECT_TEMPLATE_NAME"
	varProjectDisplayName    = "PROJECT_DISPLAYNAME"
	varProjectDescription    = "PROJECT_DESCRIPTION"
	varProjectUser           = "PROJECT_USER"
	varProjectRequestingUser = "PROJECT_REQUESTING_USER"
	varProjectAdminUser      = "PROJECT_ADMIN_USER"
	varProjectNamespace      = "PROJECT_NAMESPACE"
	varKeycloakURL           = "KEYCLOAK_URL"
)

// InitTenant initializes the tenant, ie, creates the user namespace/project with rolebinding restrictions, etc. and
// if everything went fine, launch 1 go routine per other type of namespace to create. Otherwise, returns an error
// (most probably because of quota restriction)
func InitTenant(ctx context.Context, config Config, callback Callback, username, usertoken string, templateVars map[string]string) error {
	log.Debug(ctx, map[string]interface{}{"username": username}, "initializing tenant for user...")
	templs, err := LoadProcessedTemplates(ctx, config, username, templateVars)
	if err != nil {
		return err
	}
	mappedObjects, err := MapByNamespaceAndSort(templs)
	if err != nil {
		return err
	}
	userOpts := ApplyOptions{Config: config.WithToken(usertoken), Callback: callback}
	masterOpts := ApplyOptions{Config: config, Callback: callback}
	// init user namespace first, and check for errors
	for namespace, objects := range mappedObjects {
		namespaceType := tenant.GetNamespaceType(namespace)
		if namespaceType == tenant.TypeUser {
			log.Debug(ctx, map[string]interface{}{"username": username, "namespace": namespace}, "initializing namespace for user...")
			delete(mappedObjects, namespace) // remove the ns entry so it won't be processed again afterwards
			err = initUserNamespace(ctx, namespace, objects, masterOpts, userOpts)
			if err != nil {
				log.Info(ctx, map[string]interface{}{"error_message": err, "error_type": reflect.TypeOf(err)}, "error occurred while initializing user namespace")
				// enrich the error with the namespace
				return enrichError(ctx, err, namespace)
			}
		}
	}
	// if user namespace was initialized, then proceed with other namespaces in separate go routines...
	var wg sync.WaitGroup
	wg.Add(len(mappedObjects))
	for namespace, objects := range mappedObjects {
		go func(namespace string, objects []map[interface{}]interface{}, opts ApplyOptions) {
			defer wg.Done()
			err := applyProcessed(ctx, objects, opts)
			if err != nil {
				log.Error(ctx, map[string]interface{}{
					"namespace": namespace,
					"err":       err,
				}, "error dsaas project")
			}
		}(namespace, objects, masterOpts)
	}
	wg.Wait()
	return nil
}

func enrichError(ctx context.Context, err error, namespace string) error {
	switch err := err.(type) {
	case errors.NamespaceConflictError:
		log.Debug(ctx, map[string]interface{}{"namespace": namespace}, "adding namespace info in NamespaceConflict error")
		err.Namespace = namespace
		return err // return the enriched error
	case errors.ForbiddenError:
		err.Namespace = namespace
		return err // return the enriched error
	default:
		if errs.Cause(err) != nil {
			return enrichError(ctx, errs.Cause(err), namespace)
		}
	}
	return err
}

func initUserNamespace(ctx context.Context, namespace string, objects []map[interface{}]interface{}, opts, userOpts ApplyOptions) error {
	err := applyProcessed(ctx, Filter(objects, IsOfKind(ValKindProjectRequest, ValKindNamespace)), userOpts)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"namespace": namespace,
			"err":       err,
		}, "error during the initialization of the user project (project creation)")
		return errs.Wrapf(err, "error during the initialization of the user project (project creation)")
	}
	err = applyProcessed(ctx, Filter(objects, IsOfKind(ValKindRoleBindingRestriction)), opts)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"namespace": namespace,
			"err":       err,
		}, "error during the initialization of the user project (role binding restrictions)")
		return errs.Wrapf(err, "error during the initialization of the user project (role binding restrictions)")
	}
	err = applyProcessed(ctx, Filter(objects, IsNotOfKind(ValKindProjectRequest, ValKindNamespace, ValKindRoleBindingRestriction)), userOpts)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"namespace": namespace,
			"err":       err,
		}, "error during the initialization of the user project (other)")
		return errs.Wrapf(err, "error during the initialization of the user project (other)")
	}
	_, err = apply(
		ctx,
		CreateAdminRoleBinding(namespace),
		"DELETE",
		opts.WithCallback(
			func(statusCode int, method string, request, response map[interface{}]interface{}) (string, map[interface{}]interface{}) {
				log.Info(ctx, map[string]interface{}{
					"status":    statusCode,
					"method":    method,
					"namespace": GetNamespace(request),
					"name":      GetName(request),
					"kind":      GetKind(request),
				}, "resource requested")
				return "", nil
			},
		),
	)
	if err != nil {
		log.Error(ctx, map[string]interface{}{
			"namespace": namespace,
			"err":       err,
		}, "unable to remove admin role from project")
	}
	return errs.Wrapf(err, "unable to remove adming role from project")
}

func UpdateTenant(ctx context.Context, config Config, callback Callback, username string, templateVars map[string]string) error {
	templs, err := LoadProcessedTemplates(ctx, config, username, templateVars)
	if err != nil {
		return err
	}

	mapped, err := MapByNamespaceAndSort(templs)
	if err != nil {
		return err
	}
	masterOpts := ApplyOptions{Config: config, Callback: callback}
	var wg sync.WaitGroup
	wg.Add(len(mapped))
	for key, val := range mapped {
		go func(namespace string, objects []map[interface{}]interface{}, opts ApplyOptions) {
			defer wg.Done()
			output, err := executeProccessedNamespaceCMD(
				listToTemplate(
					//RemoveReplicas(
					Filter(
						objects,
						IsNotOfKind(ValKindProjectRequest),
					),
					//),
				),
				opts.WithNamespace(namespace),
			)
			if err != nil {
				log.Error(ctx, map[string]interface{}{
					"output":    output,
					"namespace": namespace,
					"error":     err,
				}, "failed")
				return
			}
			log.Info(ctx, map[string]interface{}{
				"output":    output,
				"namespace": namespace,
			}, "applied")
		}(key, val, masterOpts)
	}
	wg.Wait()
	return nil
}

func listToTemplate(objects []map[interface{}]interface{}) string {
	template := map[interface{}]interface{}{
		"apiVersion": "v1",
		"kind":       "Template",
		"objects":    objects,
	}

	b, _ := yaml.Marshal(template)
	return string(b)
}
