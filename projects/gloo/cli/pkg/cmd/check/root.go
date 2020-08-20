package check

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rotisserie/eris"
	"github.com/solo-io/gloo/projects/gloo/cli/pkg/cmd/options"
	"github.com/solo-io/gloo/projects/gloo/cli/pkg/constants"
	"github.com/solo-io/gloo/projects/gloo/cli/pkg/flagutils"
	"github.com/solo-io/gloo/projects/gloo/cli/pkg/helpers"
	v1 "github.com/solo-io/gloo/projects/gloo/pkg/api/v1"
	"github.com/solo-io/gloo/projects/gloo/pkg/defaults"
	"github.com/solo-io/go-utils/cliutils"
	"github.com/solo-io/solo-kit/pkg/api/v1/clients"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// contains method
func doesNotContain(arr []string, str string) bool {
	for _, a := range arr {
		if a == str {
			return false
		}
	}
	return true
}

func RootCmd(opts *options.Options, optionsFunc ...cliutils.OptionsFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   constants.CHECK_COMMAND.Use,
		Short: constants.CHECK_COMMAND.Short,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := CheckResources(opts)
			if err != nil {
				// Not returning error here because this shouldn't propagate as a standard CLI error, which prints usage.
				fmt.Printf("Error!")
				fmt.Printf("%s", err.Error())
				os.Exit(1)
			} else {
				fmt.Printf("No problems detected.\n")
			}
			return nil
		},
	}
	pflags := cmd.PersistentFlags()
	flagutils.AddNamespaceFlag(pflags, &opts.Metadata.Namespace)
	flagutils.AddExcludecheckFlag(pflags, &opts.Top.CheckName)
	cliutils.ApplyOptions(cmd, optionsFunc)
	return cmd
}

func CheckResources(opts *options.Options) error {
	err := checkConnection(opts.Metadata.Namespace)
	if err != nil {
		return err
	}
	deployments, _, err := getAndCheckDeployments(opts)
	if err != nil {
		return err
	}

	includePods := doesNotContain(opts.Top.CheckName, "pods")
	if includePods {
		err := checkPods(opts)
		if err != nil {
			return err
		}
	}

	settings, err := getSettings(opts)
	if err != nil {
		return err
	}

	namespaces, err := getNamespaces(settings)
	if err != nil {
		return err
	}

	knownUpstreams, err := checkUpstreams(namespaces)
	if err != nil {
		return err
	}

	includeUpstreamGroup := doesNotContain(opts.Top.CheckName, "upstreamgroup")
	if includeUpstreamGroup {
		err := checkUpstreamGroups(namespaces)
		if err != nil {
			return err
		}
	}

	knownAuthConfigs, err := checkAuthConfigs(namespaces)
	if err != nil {
		return err
	}

	includeSecrets := doesNotContain(opts.Top.CheckName, "secrets")
	if includeSecrets {
		err := checkSecrets(namespaces)
		if err != nil {
			return err
		}
	}

	err = checkVirtualServices(namespaces, knownUpstreams, knownAuthConfigs)
	if err != nil {
		return err
	}

	includeGateway := doesNotContain(opts.Top.CheckName, "gateways")
	if includeGateway {
		err := checkGateways(namespaces)
		if err != nil {
			return err
		}
	}

	includeProxy := doesNotContain(opts.Top.CheckName, "proxies")
	if includeProxy {
		err := checkProxies(opts.Top.Ctx, namespaces, opts.Metadata.Namespace, deployments)
		if err != nil {
			return err
		}
	}

	err = checkGlooePromStats(opts.Top.Ctx, opts.Metadata.Namespace, deployments)
	if err != nil {
		return err
	}

	return nil
}

func getAndCheckDeployments(opts *options.Options) (*appsv1.DeploymentList, bool, error) {
	client := helpers.MustKubeClient()
	_, err := client.CoreV1().Namespaces().Get(opts.Metadata.Namespace, metav1.GetOptions{})
	if err != nil {
		fmt.Printf("Gloo namespace does not exist")
		return nil, false, err
	}
	deployments, err := client.AppsV1().Deployments(opts.Metadata.Namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, false, err
	}
	if len(deployments.Items) == 0 {
		fmt.Printf("Gloo is not installed")
		return nil, false, nil
	}

	var errorToPrint string
	var message string
	setMessage := func(c appsv1.DeploymentCondition) {
		if c.Message != "" {
			message = fmt.Sprintf(" Message: %s", c.Message)
		}
	}

	for _, deployment := range deployments.Items {
		// possible condition types listed at https://godoc.org/k8s.io/api/apps/v1#DeploymentConditionType
		// check for each condition independently because multiple conditions will be True and DeploymentReplicaFailure
		// tends to provide the most explicit error message.
		for _, condition := range deployment.Status.Conditions {
			setMessage(condition)
			if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
				errorToPrint = fmt.Sprintf("Deployment %s in namespace %s failed to create pods!%s", deployment.Name, deployment.Namespace, message)
			}
			if errorToPrint != "" {
				fmt.Print(errorToPrint)
				return nil, false, err
			}
		}

		for _, condition := range deployment.Status.Conditions {
			setMessage(condition)
			if condition.Type == appsv1.DeploymentProgressing && condition.Status != corev1.ConditionTrue {
				errorToPrint = fmt.Sprintf("Deployment %s in namespace %s is not progressing!%s", deployment.Name, deployment.Namespace, message)
			}

			if errorToPrint != "" {
				fmt.Print(errorToPrint)
				return nil, false, err
			}
		}

		for _, condition := range deployment.Status.Conditions {
			setMessage(condition)
			if condition.Type == appsv1.DeploymentAvailable && condition.Status != corev1.ConditionTrue {
				errorToPrint = fmt.Sprintf("Deployment %s in namespace %s is not available!%s", deployment.Name, deployment.Namespace, message)
			}

			if errorToPrint != "" {
				fmt.Print(errorToPrint)
				return nil, false, err
			}
		}

		for _, condition := range deployment.Status.Conditions {
			if condition.Type != appsv1.DeploymentAvailable &&
				condition.Type != appsv1.DeploymentReplicaFailure &&
				condition.Type != appsv1.DeploymentProgressing {
				fmt.Printf("Note: Unhandled deployment condition %s", condition.Type)
			}
		}
	}
	return deployments, true, nil
}

func checkPods(opts *options.Options) error {
	client := helpers.MustKubeClient()
	pods, err := client.CoreV1().Pods(opts.Metadata.Namespace).List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		for _, condition := range pod.Status.Conditions {
			var errorToPrint string
			var message string

			if condition.Message != "" {
				message = fmt.Sprintf(" Message: %s", condition.Message)
			}

			// if condition is not met and the pod is not completed
			conditionNotMet := condition.Status != corev1.ConditionTrue && condition.Reason != "PodCompleted"

			// possible condition types listed at https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-conditions
			switch condition.Type {
			case corev1.PodScheduled:
				if conditionNotMet {
					errorToPrint = fmt.Sprintf("Pod %s in namespace %s is not yet scheduled!%s", pod.Name, pod.Namespace, message)
				}
			case corev1.PodReady:
				if conditionNotMet {
					errorToPrint = fmt.Sprintf("Pod %s in namespace %s is not ready!%s", pod.Name, pod.Namespace, message)
				}
			case corev1.PodInitialized:
				if conditionNotMet {
					errorToPrint = fmt.Sprintf("Pod %s in namespace %s is not yet initialized!%s", pod.Name, pod.Namespace, message)
				}
			case corev1.PodReasonUnschedulable:
				if conditionNotMet {
					errorToPrint = fmt.Sprintf("Pod %s in namespace %s is unschedulable!%s", pod.Name, pod.Namespace, message)
				}
			case corev1.ContainersReady:
				if conditionNotMet {
					errorToPrint = fmt.Sprintf("Not all containers in pod %s in namespace %s are ready!%s", pod.Name, pod.Namespace, message)
				}
			default:
				fmt.Printf("Note: Unhandled pod condition %s", condition.Type)
			}

			if errorToPrint != "" {
				fmt.Print(errorToPrint)
				return err
			}
		}
	}
	return nil
}

func getSettings(opts *options.Options) (*v1.Settings, error) {
	client := helpers.MustNamespacedSettingsClient(opts.Metadata.GetNamespace())
	return client.Read(opts.Metadata.Namespace, defaults.SettingsName, clients.ReadOpts{})
}

func getNamespaces(settings *v1.Settings) ([]string, error) {
	if settings.WatchNamespaces != nil {
		return settings.WatchNamespaces, nil
	}

	return helpers.GetNamespaces()
}

func checkUpstreams(namespaces []string) ([]string, error) {
	var knownUpstreams []string
	for _, ns := range namespaces {
		upstreams, err := helpers.MustNamespacedUpstreamClient(ns).List(ns, clients.ListOpts{})
		if err != nil {
			return nil, err
		}
		for _, upstream := range upstreams {
			if upstream.Status.GetState() == core.Status_Rejected {
				fmt.Printf("Found rejected upstream: %s", renderMetadata(upstream.GetMetadata()))
				fmt.Printf("Reason: %s", upstream.Status.Reason)
				return nil, nil
			}
			if upstream.Status.GetState() == core.Status_Warning {
				fmt.Printf("Found upstream with warnings: %s", renderMetadata(upstream.GetMetadata()))
				fmt.Printf("Reason: %s", upstream.Status.Reason)
				return nil, nil
			}
			knownUpstreams = append(knownUpstreams, renderMetadata(upstream.GetMetadata()))
		}
	}
	return knownUpstreams, nil
}

func checkUpstreamGroups(namespaces []string) error {
	for _, ns := range namespaces {
		upstreamGroups, err := helpers.MustNamespacedUpstreamGroupClient(ns).List(ns, clients.ListOpts{})
		if err != nil {
			return err
		}
		for _, upstreamGroup := range upstreamGroups {
			if upstreamGroup.Status.GetState() == core.Status_Rejected {
				fmt.Printf("Found rejected upstream group: %s", renderMetadata(upstreamGroup.GetMetadata()))
				fmt.Printf("Reason: %s", upstreamGroup.Status.Reason)
				return nil
			}
			if upstreamGroup.Status.GetState() == core.Status_Warning {
				fmt.Printf("Found upstream group with warnings: %s", renderMetadata(upstreamGroup.GetMetadata()))
				fmt.Printf("Reason: %s", upstreamGroup.Status.Reason)
				return nil
			}
		}
	}
	return nil
}

func checkAuthConfigs(namespaces []string) ([]string, error) {
	var knownAuthConfigs []string
	for _, ns := range namespaces {
		authConfigs, err := helpers.MustNamespacedAuthConfigClient(ns).List(ns, clients.ListOpts{})
		if err != nil {
			return nil, err
		}
		for _, authConfig := range authConfigs {
			if authConfig.Status.GetState() == core.Status_Rejected {
				fmt.Printf("Found rejected auth config: %s", renderMetadata(authConfig.GetMetadata()))
				fmt.Printf("Reason: %s", authConfig.Status.Reason)
				return nil, nil
			}
			if authConfig.Status.GetState() == core.Status_Warning {
				fmt.Printf("Found auth config with warnings: %s", renderMetadata(authConfig.GetMetadata()))
				fmt.Printf("Reason: %s", authConfig.Status.Reason)
				return nil, nil
			}
			knownAuthConfigs = append(knownAuthConfigs, renderMetadata(authConfig.GetMetadata()))
		}
	}
	return knownAuthConfigs, nil
}

func checkVirtualServices(namespaces, knownUpstreams []string, knownAuthConfigs []string) error {
	for _, ns := range namespaces {
		virtualServices, err := helpers.MustNamespacedVirtualServiceClient(ns).List(ns, clients.ListOpts{})
		if err != nil {
			return err
		}
		for _, virtualService := range virtualServices {
			if virtualService.Status.GetState() == core.Status_Rejected {
				fmt.Printf("Found rejected virtual service: %s", renderMetadata(virtualService.GetMetadata()))
				fmt.Printf("Reason: %s", virtualService.Status.GetReason())
				return nil
			}
			if virtualService.Status.GetState() == core.Status_Warning {
				fmt.Printf("Found virtual service with warnings: %s", renderMetadata(virtualService.GetMetadata()))
				fmt.Printf("Reason: %s", virtualService.Status.GetReason())
				return nil
			}
			for _, route := range virtualService.GetVirtualHost().GetRoutes() {
				if route.GetRouteAction() != nil {
					if route.GetRouteAction().GetSingle() != nil {
						us := route.GetRouteAction().GetSingle()
						if us.GetUpstream() != nil {
							if !cliutils.Contains(knownUpstreams, renderRef(us.GetUpstream())) {
								fmt.Printf("Virtual service references unknown upstream:")
								fmt.Printf("  Virtual service: %s", renderMetadata(virtualService.GetMetadata()))
								fmt.Printf("  Upstream: %s", renderRef(us.GetUpstream()))
								return nil
							}
						}
					}
				}
			}
			acRef := virtualService.GetVirtualHost().GetOptions().GetExtauth().GetConfigRef()
			if acRef != nil && !cliutils.Contains(knownAuthConfigs, renderRef(acRef)) {
				fmt.Printf("Virtual service references unknown auth config:")
				fmt.Printf("  Virtual service: %s", renderMetadata(virtualService.GetMetadata()))
				fmt.Printf("  Auth Config: %s", renderRef(acRef))
				return nil
			}
		}
	}
	return nil
}

func checkGateways(namespaces []string) error {
	for _, ns := range namespaces {
		gateways, err := helpers.MustNamespacedGatewayClient(ns).List(ns, clients.ListOpts{})
		if err != nil {
			return err
		}
		for _, gateway := range gateways {
			if gateway.Status.GetState() == core.Status_Rejected {
				fmt.Printf("Found rejected gateway: %s", renderMetadata(gateway.GetMetadata()))
				fmt.Printf("Reason: %s", gateway.Status.Reason)
				return nil
			}
			if gateway.Status.GetState() == core.Status_Warning {
				fmt.Printf("Found gateway with warnings: %s", renderMetadata(gateway.GetMetadata()))
				fmt.Printf("Reason: %s", gateway.Status.Reason)
				return nil
			}
		}
	}
	return nil
}

func checkProxies(ctx context.Context, namespaces []string, glooNamespace string, deployments *appsv1.DeploymentList) error {
	for _, ns := range namespaces {
		proxies, err := helpers.MustNamespacedProxyClient(ns).List(ns, clients.ListOpts{})
		if err != nil {
			return err
		}
		for _, proxy := range proxies {
			if proxy.Status.GetState() == core.Status_Rejected {
				fmt.Printf("Found rejected proxy: %s", renderMetadata(proxy.GetMetadata()))
				fmt.Printf("Reason: %s", proxy.Status.Reason)
				return nil
			}
			if proxy.Status.GetState() == core.Status_Warning {
				fmt.Printf("Found proxy with warnings: %s", renderMetadata(proxy.GetMetadata()))
				fmt.Printf("Reason: %s", proxy.Status.Reason)
				return nil
			}
		}
	}

	return checkProxiesPromStats(ctx, glooNamespace, deployments)

}

func checkSecrets(namespaces []string) error {
	client := helpers.MustSecretClientWithOptions(5*time.Second, namespaces)

	for _, ns := range namespaces {
		_, err := client.List(ns, clients.ListOpts{})
		if err != nil {
			return err
		}
		// currently this would only find syntax errors
	}
	return nil
}

func renderMetadata(metadata core.Metadata) string {
	return renderNamespaceName(metadata.Namespace, metadata.Name)
}

func renderRef(ref *core.ResourceRef) string {
	return renderNamespaceName(ref.Namespace, ref.Name)
}

func renderNamespaceName(namespace, name string) string {
	return fmt.Sprintf("%s %s", namespace, name)
}

// Checks whether the cluster that the kubeconfig points at is available
// The timeout for the kubernetes client is set to a low value to notify the user of the failure
func checkConnection(ns string) error {
	client, err := helpers.GetKubernetesClientWithTimeout(5 * time.Second)
	if err != nil {
		return eris.Wrapf(err, "Could not get kubernetes client")
	}
	_, err = client.CoreV1().Namespaces().Get(ns, metav1.GetOptions{})
	if err != nil {
		return eris.Wrapf(err, "Could not communicate with kubernetes cluster")
	}
	return nil
}
