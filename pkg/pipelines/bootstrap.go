package pipelines

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	v1rbac "k8s.io/api/rbac/v1"

	"github.com/mitchellh/go-homedir"
	"github.com/openshift/odo/pkg/pipelines/eventlisteners"
	"github.com/openshift/odo/pkg/pipelines/meta"
	"github.com/openshift/odo/pkg/pipelines/routes"
	"github.com/openshift/odo/pkg/pipelines/tasks"
	"github.com/openshift/odo/pkg/pipelines/triggers"
	"sigs.k8s.io/yaml"
)

var (
	dockerSecretName     = "regcred"
	saName               = "pipeline"
	roleName             = "tekton-triggers-openshift-demo"
	roleBindingName      = "tekton-triggers-openshift-binding"
	devRoleBindingName   = "pipeline-admin-dev"
	stageRoleBindingName = "pipeline-admin-stage"

	// PolicyRules to be bound to service account
	rules = []v1rbac.PolicyRule{
		v1rbac.PolicyRule{
			APIGroups: []string{"tekton.dev"},
			Resources: []string{"eventlisteners", "triggerbindings", "triggertemplates", "tasks", "taskruns"},
			Verbs:     []string{"get"},
		},
		v1rbac.PolicyRule{
			APIGroups: []string{"tekton.dev"},
			Resources: []string{"pipelineruns", "pipelineresources", "taskruns"},
			Verbs:     []string{"create"},
		},
	}
)

// BootstrapOptions is a struct that provides the optional flags
type BootstrapOptions struct {
	DeploymentPath           string
	GithubToken              string
	GitRepo                  string
	InternalRegistryHostname string
	ImageRepo                string
	Prefix                   string
	QuayAuthFileName         string
	QuayUserName             string
	SkipChecks               bool
}

// Bootstrap is the main driver for getting OpenShift pipelines for GitOps
// configured with a basic configuration.
func Bootstrap(o *BootstrapOptions) error {

	if !o.SkipChecks {
		installed, err := checkTektonInstall()
		if err != nil {
			return fmt.Errorf("failed to run Tekton Pipelines installation check: %w", err)
		}
		if !installed {
			return errors.New("failed due to Tekton Pipelines or Triggers are not installed")
		}
	}

	// Validate image repo
	isInternalRegistry, imageRepo, err := validateImageRepo(o)
	if err != nil {
		return err
	}

	outputs := make([]interface{}, 0)
	namespaces := namespaceNames(o.Prefix)
	for _, n := range createNamespaces(values(namespaces)) {
		outputs = append(outputs, n)
	}

	githubAuth, err := createOpaqueSecret(meta.NamespacedName(namespaces["cicd"], "github-auth"), o.GithubToken)
	if err != nil {
		return fmt.Errorf("failed to generate path to file: %w", err)
	}
	outputs = append(outputs, githubAuth)

	// Create Tasks
	tasks := tasks.Generate(githubAuth.GetName(), namespaces["cicd"], isInternalRegistry)
	for _, task := range tasks {
		outputs = append(outputs, task)
	}

	// Create trigger templates
	templates := triggers.GenerateTemplates(namespaces["cicd"], saName, imageRepo)
	for _, template := range templates {
		outputs = append(outputs, template)
	}

	// Create trigger bindings
	bindings := triggers.GenerateBindings(namespaces["cicd"])
	for _, binding := range bindings {
		outputs = append(outputs, binding)
	}

	// Create Pipelines
	outputs = append(outputs, createPipelines(namespaces, o.DeploymentPath)...)

	// Create Event Listener
	eventListener := eventlisteners.Generate(o.GitRepo, namespaces["cicd"], saName)
	outputs = append(outputs, eventListener)

	// Create route
	route := routes.Generate(namespaces["cicd"])
	outputs = append(outputs, route)

	// Create Service Account
	sa := createServiceAccount(meta.NamespacedName(namespaces["cicd"], saName))

	if isInternalRegistry {
		// add sa to outputs
		outputs = append(outputs, sa)
		// Provide access to service account for using internal registry
		internalRegistryNamespace := strings.Split(imageRepo, "/")[1]

		clientSet, err := getClientSet()
		if err != nil {
			return err
		}
		namespaceExists, err := checkNamespace(clientSet, internalRegistryNamespace)
		if err != nil {
			return err
		}
		if !namespaceExists {
			outputs = append(outputs, createNamespace(internalRegistryNamespace))
		}

		outputs = append(outputs, createRoleBinding(meta.NamespacedName(internalRegistryNamespace, "internal-registry-binding"), sa, "ClusterRole", "edit"))
	} else {
		// Add secret to service account if external registry is used
		dockerSecret, err := createDockerSecret(o.QuayAuthFileName, namespaces["cicd"])
		if err != nil {
			return err
		}
		outputs = append(outputs, dockerSecret)
		// add secret and sa to outputs
		outputs = append(outputs, addSecretToSA(sa, dockerSecretName))
	}

	//  Create Role, Role Bindings, and ClusterRole Bindings
	outputs = append(outputs, createRoleBindings(namespaces, sa)...)

	return marshalOutputs(os.Stdout, outputs)
}

func createRoleBindings(ns map[string]string, sa *corev1.ServiceAccount) []interface{} {
	out := make([]interface{}, 0)

	role := createRole(meta.NamespacedName(ns["cicd"], roleName), rules)
	out = append(out, role)
	out = append(out, createRoleBinding(meta.NamespacedName(ns["cicd"], roleBindingName), sa, role.Kind, role.Name))
	out = append(out, createRoleBinding(meta.NamespacedName(ns["cicd"], "edit-clusterrole-binding"), sa, "ClusterRole", "edit"))
	out = append(out, createRoleBinding(meta.NamespacedName(ns["dev"], devRoleBindingName), sa, "ClusterRole", "edit"))
	out = append(out, createRoleBinding(meta.NamespacedName(ns["stage"], stageRoleBindingName), sa, "ClusterRole", "edit"))

	return out
}

func createPipelines(ns map[string]string, deploymentPath string) []interface{} {
	out := make([]interface{}, 0)
	out = append(out, createDevCIPipeline(meta.NamespacedName(ns["cicd"], "dev-ci-pipeline")))
	out = append(out, createStageCIPipeline(meta.NamespacedName(ns["cicd"], "stage-ci-pipeline"), ns["stage"]))
	out = append(out, createDevCDPipeline(meta.NamespacedName(ns["cicd"], "dev-cd-pipeline"), deploymentPath, ns["dev"]))
	out = append(out, createStageCDPipeline(meta.NamespacedName(ns["cicd"], "stage-cd-pipeline"), ns["stage"]))
	return out

}

// createDockerSecret creates Docker secret
func createDockerSecret(quayIOAuthFilename, ns string) (*corev1.Secret, error) {

	authJSONPath, err := homedir.Expand(quayIOAuthFilename)
	if err != nil {
		return nil, fmt.Errorf("failed to generate path to file: %w", err)
	}

	f, err := os.Open(authJSONPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read docker file '%s' : %w", authJSONPath, err)
	}
	defer f.Close()

	dockerSecret, err := createDockerConfigSecret(meta.NamespacedName(ns, dockerSecretName), f)
	if err != nil {
		return nil, err
	}

	return dockerSecret, nil

}

// create and invoke a Tekton Checker
func checkTektonInstall() (bool, error) {
	tektonChecker, err := newTektonChecker()
	if err != nil {
		return false, err
	}
	return tektonChecker.checkInstall()
}

func values(m map[string]string) []string {
	values := []string{}
	for _, v := range m {
		values = append(values, v)

	}
	return values
}

// marshalOutputs marshal outputs to given writer
func marshalOutputs(out io.Writer, outputs []interface{}) error {
	for _, r := range outputs {
		data, err := yaml.Marshal(r)
		if err != nil {
			return fmt.Errorf("failed to marshal data: %w", err)
		}
		_, err = fmt.Fprintf(out, "%s---\n", data)
		if err != nil {
			return fmt.Errorf("failed to write data: %w", err)
		}
	}
	return nil
}

// validateImageRepo validates the input image repo.  It determines if it is
// for internal registry and prepend internal registry hostname if neccessary.
func validateImageRepo(o *BootstrapOptions) (bool, string, error) {
	components := strings.Split(o.ImageRepo, "/")
	errorMsg := "failed to parse image repo:%s, expected image repository in the form <registry>/<username>/<repository> or <project>/<app> for internal registry"

	// repo url has minimum of 2 components
	if len(components) < 2 {
		return false, "", fmt.Errorf(errorMsg, o.ImageRepo)
	}

	// check for spaces
	for _, v := range components {
		if strings.TrimSpace(v) == "" {
			return false, "", fmt.Errorf(errorMsg, o.ImageRepo)
		}
	}

	if len(components) == 2 {
		if components[0] == "docker.io" || components[0] == "quay.io" {
			// we recconize docker.io and quay.io.  It is missing one component
			return false, "", fmt.Errorf(errorMsg, o.ImageRepo)
		}
		// We have format like <project>/<app> which is an internal registry.
		// We prepend the internal registry hostname.
		return true, o.InternalRegistryHostname + "/" + o.ImageRepo, nil
	}

	// Check the first component to see if it is an internal registry
	if len(components) == 3 {
		return components[0] == o.InternalRegistryHostname, o.ImageRepo, nil
	}

	// > 3 components.  invalid repo
	return false, "", fmt.Errorf(errorMsg, o.ImageRepo)
}
