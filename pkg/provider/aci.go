/*
Copyright (c) Microsoft Corporation.
Licensed under the Apache 2.0 license.
*/
package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	azaci "github.com/Azure/azure-sdk-for-go/services/containerinstance/mgmt/2021-10-01/containerinstance"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/virtual-kubelet/azure-aci/client/aci"
	"github.com/virtual-kubelet/azure-aci/pkg/analytics"
	"github.com/virtual-kubelet/azure-aci/pkg/auth"
	client2 "github.com/virtual-kubelet/azure-aci/pkg/client"
	"github.com/virtual-kubelet/azure-aci/pkg/metrics"
	"github.com/virtual-kubelet/azure-aci/pkg/validation"
	"github.com/virtual-kubelet/node-cli/manager"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// The service account secret mount path.
	serviceAccountSecretMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

	virtualKubeletDNSNameLabel = "virtualkubelet.io/dnsnamelabel"

	subnetDelegationService = "Microsoft.ContainerInstance/containerGroups"
	// Parameter names defined in azure file CSI driver, refer to
	// https://github.com/kubernetes-sigs/azurefile-csi-driver/blob/master/docs/driver-parameters.md
	azureFileShareName  = "shareName"
	azureFileSecretName = "secretName"
	// AzureFileDriverName is the name of the CSI driver for Azure File
	AzureFileDriverName         = "file.csi.azure.com"
	azureFileStorageAccountName = "azurestorageaccountname"
	azureFileStorageAccountKey  = "azurestorageaccountkey"

	LogAnalyticsMetadataKeyNodeName          string = "node-name"
	LogAnalyticsMetadataKeyClusterResourceID string = "cluster-resource-id"
)

const (
	gpuResourceName   = "nvidia.com/gpu"
	gpuTypeAnnotation = "virtual-kubelet.io/gpu-type"
)

const (
	statusReasonPodDeleted            = "NotFound"
	statusMessagePodDeleted           = "The pod may have been deleted from the provider"
	containerExitCodePodDeleted int32 = 0
)

// ACIProvider implements the virtual-kubelet provider interface and communicates with Azure's ACI APIs.
type ACIProvider struct {
	azClientsAPIs            client2.AzClientsInterface
	resourceManager          *manager.ResourceManager
	containerGroupExtensions []*client2.Extension

	resourceGroup      string
	region             string
	nodeName           string
	operatingSystem    string
	cpu                string
	memory             string
	pods               string
	gpu                string
	gpuSKUs            []azaci.GpuSku
	internalIP         string
	daemonEndpointPort int32
	diagnostics        *azaci.ContainerGroupDiagnostics
	subnetName         string
	subnetCIDR         string
	vnetSubscriptionID string
	vnetName           string
	vnetResourceGroup  string
	clusterDomain      string
	kubeDNSIP          string
	tracker            *PodsTracker

	*metrics.ACIPodMetricsProvider
}

// AuthConfig is the secret returned from an ImageRegistryCredential
type AuthConfig struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	Email         string `json:"email,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
}

// See https://learn.microsoft.com/en-us/azure/container-instances/container-instances-region-availability
var validAciRegions = []string{
	"australiaeast",
	"australiasoutheast",
	"brazilsouth",
	"canadacentral",
	"canadaeast",
	"centralindia",
	"centralus",
	"centraluseuap",
	"eastasia",
	"eastus",
	"eastus2",
	"eastus2euap",
	"francecentral",
	"germanywestcentral",
	"japaneast",
	"japanwest",
	"jioindiawest",
	"koreacentral",
	"northcentralus",
	"northeurope",
	"norwayeast",
	"norwaywest",
	"southafricanorth",
	"southcentralus",
	"southindia",
	"southeastasia",
	"swedencentral",
	"swedensouth",
	"switzerlandnorth",
	"switzerlandwest",
	"uaenorth",
	"uksouth",
	"ukwest",
	"westcentralus",
	"westeurope",
	"westindia",
	"westus",
	"westus2",
	"westus3",
	"usgovvirginia",
	"usgovarizona",
}

// isValidACIRegion checks to make sure we're using a valid ACI region
func isValidACIRegion(region string) bool {
	regionLower := strings.ToLower(region)
	regionTrimmed := strings.Replace(regionLower, " ", "", -1)

	for _, validRegion := range validAciRegions {
		if regionTrimmed == validRegion {
			return true
		}
	}

	return false
}

// NewACIProvider creates a new ACIProvider.
func NewACIProvider(ctx context.Context, config string, azConfig auth.Config, azAPIs client2.AzClientsInterface, rm *manager.ResourceManager, nodeName, operatingSystem string, internalIP string, daemonEndpointPort int32, clusterDomain string) (*ACIProvider, error) {
	var p ACIProvider
	var err error

	if config != "" {
		f, err := os.Open(config)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		if err := p.loadConfig(f); err != nil {
			return nil, err
		}
	}

	p.azClientsAPIs = azAPIs
	p.resourceManager = rm
	p.clusterDomain = clusterDomain
	p.operatingSystem = operatingSystem
	p.nodeName = nodeName
	p.internalIP = internalIP
	p.daemonEndpointPort = daemonEndpointPort

	if azConfig.AKSCredential != nil {
		p.resourceGroup = azConfig.AKSCredential.ResourceGroup
		p.region = azConfig.AKSCredential.Region

		p.vnetName = azConfig.AKSCredential.VNetName
		p.vnetResourceGroup = azConfig.AKSCredential.VNetResourceGroup
	}

	if p.vnetResourceGroup == "" {
		p.vnetResourceGroup = p.resourceGroup
	}
	// If the log analytics file has been specified, load workspace credentials from the file
	if logAnalyticsAuthFile := os.Getenv("LOG_ANALYTICS_AUTH_LOCATION"); logAnalyticsAuthFile != "" {
		p.diagnostics, err = analytics.NewContainerGroupDiagnosticsFromFile(logAnalyticsAuthFile)
		if err != nil {
			return nil, err
		}
	}

	// If we have both the log analytics workspace id and key, add them to the provider
	// Environment variables overwrite the values provided in the file
	if logAnalyticsID := os.Getenv("LOG_ANALYTICS_ID"); logAnalyticsID != "" {
		if logAnalyticsKey := os.Getenv("LOG_ANALYTICS_KEY"); logAnalyticsKey != "" {
			p.diagnostics, err = analytics.NewContainerGroupDiagnostics(logAnalyticsID, logAnalyticsKey)
			if err != nil {
				return nil, err
			}
		}
	}

	if clusterResourceID := os.Getenv("CLUSTER_RESOURCE_ID"); clusterResourceID != "" {
		if p.diagnostics != nil && p.diagnostics.LogAnalytics != nil {
			p.diagnostics.LogAnalytics.LogType = azaci.LogAnalyticsLogTypeContainerInsights
			p.diagnostics.LogAnalytics.Metadata = map[string]*string{
				LogAnalyticsMetadataKeyClusterResourceID: &clusterResourceID,
				LogAnalyticsMetadataKeyNodeName:          &nodeName,
			}
		}
	}

	if rg := os.Getenv("ACI_RESOURCE_GROUP"); rg != "" {
		p.resourceGroup = rg
	}
	if p.resourceGroup == "" {
		return nil, errors.New("Resource group can not be empty please set ACI_RESOURCE_GROUP")
	}

	if r := os.Getenv("ACI_REGION"); r != "" {
		p.region = r
	}
	if p.region == "" {
		return nil, errors.New("Region can not be empty please set ACI_REGION")
	}

	if r := p.region; !isValidACIRegion(r) {
		unsupportedRegionMessage := fmt.Sprintf("Region %s is invalid. Current supported regions are: %s",
			r, strings.Join(validAciRegions, ", "))
		return nil, errors.New(unsupportedRegionMessage)
	}

	if err := p.setupNodeCapacity(ctx); err != nil {
		return nil, err
	}

	if err := p.setVNETConfig(ctx, &azConfig); err != nil {
		return nil, err
	}

	p.ACIPodMetricsProvider = metrics.NewACIPodMetricsProvider(nodeName, p.resourceGroup, p.resourceManager, p.azClientsAPIs)
	return &p, err
}

func addAzureAttributes(ctx context.Context, span trace.Span, p *ACIProvider) context.Context {
	return span.WithFields(ctx, log.Fields{
		"azure.resourceGroup": p.resourceGroup,
		"azure.region":        p.region,
	})
}

// CreatePod accepts a Pod definition and creates
// an ACI deployment
func (p *ACIProvider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	var err error
	ctx, span := trace.StartSpan(ctx, "aci.CreatePod")
	defer span.End()
	ctx = addAzureAttributes(ctx, span, p)

	cg := &client2.ContainerGroupWrapper{
		ContainerGroupPropertiesWrapper: &client2.ContainerGroupPropertiesWrapper{
			ContainerGroupProperties: &azaci.ContainerGroupProperties{},
		},
	}

	cg.Location = &p.region
	cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.RestartPolicy = azaci.ContainerGroupRestartPolicy(pod.Spec.RestartPolicy)
	cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.OsType = azaci.OperatingSystemTypes(p.operatingSystem)

	// get containers
	containers, err := p.getContainers(pod)
	if err != nil {
		return err
	}
	// get registry creds
	creds, err := p.getImagePullSecrets(pod)
	if err != nil {
		return err
	}
	// get volumes
	volumes, err := p.getVolumes(ctx, pod)
	if err != nil {
		return err

	}

	// get initContainers
	initContainers, err := p.getInitContainers(ctx, pod)
	if err != nil {
		return err
	}

	// assign all the things
	cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.InitContainers = &initContainers
	cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.Containers = containers
	cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.Volumes = &volumes
	cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.ImageRegistryCredentials = creds
	cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.Diagnostics = p.getDiagnostics(pod)

	filterWindowsServiceAccountSecretVolume(ctx, p.operatingSystem, cg)

	// create ipaddress if containerPort is used
	count := 0
	for _, container := range *containers {
		count = count + len(*container.Ports)
	}
	ports := make([]azaci.Port, 0, count)
	for c := range *containers {
		containerPorts := ((*containers)[c]).Ports
		for p := range *containerPorts {
			ports = append(ports, azaci.Port{
				Port:     (*containerPorts)[p].Port,
				Protocol: azaci.ContainerGroupNetworkProtocolTCP,
			})
		}
	}
	if len(ports) > 0 && p.subnetName == "" {
		cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.IPAddress = &azaci.IPAddress{
			Ports: &ports,
			Type:  azaci.ContainerGroupIPAddressTypePublic,
		}

		if dnsNameLabel := pod.Annotations[virtualKubeletDNSNameLabel]; dnsNameLabel != "" {
			cg.ContainerGroupPropertiesWrapper.ContainerGroupProperties.IPAddress.DNSNameLabel = &dnsNameLabel
		}
	}

	podUID := string(pod.UID)
	podCreationTimestamp := pod.CreationTimestamp.String()
	cg.Tags = map[string]*string{
		"PodName":           &pod.Name,
		"ClusterName":       &pod.ClusterName,
		"NodeName":          &pod.Spec.NodeName,
		"Namespace":         &pod.Namespace,
		"UID":               &podUID,
		"CreationTimestamp": &podCreationTimestamp,
	}

	p.amendVnetResources(ctx, *cg, pod)

	log.G(ctx).Infof("start creating pod %v", pod.Name)
	// TODO: Run in a go routine to not block workers, and use tracker.UpdatePodStatus() based on result.
	return p.azClientsAPIs.CreateContainerGroup(ctx, p.resourceGroup, pod.Namespace, pod.Name, cg)
}

func (p *ACIProvider) getDiagnostics(pod *v1.Pod) *azaci.ContainerGroupDiagnostics {
	if p.diagnostics != nil && p.diagnostics.LogAnalytics != nil && p.diagnostics.LogAnalytics.LogType == azaci.LogAnalyticsLogTypeContainerInsights {
		d := *p.diagnostics
		uID := string(pod.ObjectMeta.UID)
		d.LogAnalytics.Metadata[aci.LogAnalyticsMetadataKeyPodUUID] = &uID
		return &d
	}
	return p.diagnostics
}

func containerGroupName(podNS, podName string) string {
	return fmt.Sprintf("%s-%s", podNS, podName)
}

// UpdatePod is a noop, ACI currently does not support live updates of a pod.
func (p *ACIProvider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	return nil
}

// DeletePod deletes the specified pod out of ACI.
func (p *ACIProvider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "aci.DeletePod")
	defer span.End()
	ctx = addAzureAttributes(ctx, span, p)

	log.G(ctx).Infof("start deleting pod %v", pod.Name)
	// TODO: Run in a go routine to not block workers.
	return p.deleteContainerGroup(ctx, pod.Namespace, pod.Name)
}

func (p *ACIProvider) deleteContainerGroup(ctx context.Context, podNS, podName string) error {
	ctx, span := trace.StartSpan(ctx, "aci.deleteContainerGroup")
	defer span.End()
	ctx = addAzureAttributes(ctx, span, p)

	cgName := containerGroupName(podNS, podName)

	err := p.azClientsAPIs.DeleteContainerGroup(ctx, p.resourceGroup, cgName)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to delete container group %v", cgName)
		return err
	}

	if p.tracker != nil {
		// Delete is not a sync API on ACI yet, but will assume with current implementation that termination is completed. Also, till gracePeriod is supported.
		updateErr := p.tracker.UpdatePodStatus(ctx,
			podNS,
			podName,
			func(podStatus *v1.PodStatus) {
				now := metav1.NewTime(time.Now())
				for i := range podStatus.ContainerStatuses {
					if podStatus.ContainerStatuses[i].State.Running == nil {
						continue
					}

					podStatus.ContainerStatuses[i].State.Terminated = &v1.ContainerStateTerminated{
						ExitCode:    containerExitCodePodDeleted,
						Reason:      statusReasonPodDeleted,
						Message:     statusMessagePodDeleted,
						FinishedAt:  now,
						StartedAt:   podStatus.ContainerStatuses[i].State.Running.StartedAt,
						ContainerID: podStatus.ContainerStatuses[i].ContainerID,
					}
					podStatus.ContainerStatuses[i].State.Running = nil
				}
			},
			false,
		)

		if updateErr != nil && !errdefs.IsNotFound(updateErr) {
			log.G(ctx).WithError(updateErr).Errorf("failed to update termination status for cg %v", cgName)
		}
	}

	return nil
}

// GetPod returns a pod by name that is running inside ACI
// returns nil if a pod by that name is not found.
func (p *ACIProvider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "aci.GetPod")
	defer span.End()
	ctx = addAzureAttributes(ctx, span, p)

	cg, err := p.azClientsAPIs.GetContainerGroupInfo(ctx, p.resourceGroup, namespace, name, p.nodeName)
	if err != nil {
		return nil, err
	}

	err = validation.ValidateContainerGroup(cg)
	if err != nil {
		return nil, err
	}
	return p.containerGroupToPod(cg)
}

// GetContainerLogs returns the logs of a pod by name that is running inside ACI.
func (p *ACIProvider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	ctx, span := trace.StartSpan(ctx, "aci.GetContainerLogs")
	defer span.End()
	ctx = addAzureAttributes(ctx, span, p)

	cg, err := p.azClientsAPIs.GetContainerGroupInfo(ctx, p.resourceGroup, namespace, podName, p.nodeName)
	if err != nil {
		return nil, err
	}

	// get logs from cg
	logContent, err := p.azClientsAPIs.ListLogs(ctx, p.resourceGroup, *cg.Name, containerName, opts)
	if err != nil {
		return nil, err
	}
	if logContent != nil {
		logStr := *logContent
		return io.NopCloser(strings.NewReader(logStr)), nil
	}
	return nil, nil
}

// GetPodFullName as defined in the provider context
func (p *ACIProvider) GetPodFullName(namespace string, pod string) string {
	return fmt.Sprintf("%s-%s", namespace, pod)
}

// RunInContainer executes a command in a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *ACIProvider) RunInContainer(ctx context.Context, namespace, name, container string, cmd []string, attach api.AttachIO) error {
	logger := log.G(ctx).WithField("method", "RunInContainer")
	ctx, span := trace.StartSpan(ctx, "aci.RunInContainer")
	defer span.End()
	ctx = addAzureAttributes(ctx, span, p)

	out := attach.Stdout()
	if out != nil {
		defer out.Close()
	}

	cg, err := p.azClientsAPIs.GetContainerGroupInfo(ctx, p.resourceGroup, namespace, name, p.nodeName)
	if err != nil {
		return err
	}

	// Set default terminal size
	cols := int32(60)
	rows := int32(120)
	cmdParam := strings.Join(cmd, " ")
	req := azaci.ContainerExecRequest{
		Command: &cmdParam,
		TerminalSize: &azaci.ContainerExecRequestTerminalSize{
			Cols: &cols,
			Rows: &rows,
		},
	}

	xcrsp, err := p.azClientsAPIs.ExecuteContainerCommand(ctx, p.resourceGroup, *cg.Name, container, req)
	if err != nil {
		return err
	}

	wsURI := *xcrsp.WebSocketURI
	password := *xcrsp.Password

	c, _, err := websocket.DefaultDialer.Dial(wsURI, nil)
	if err != nil {
		return err
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(password)); err != nil { // Websocket password needs to be sent before WS terminal is active
		return err
	}

	// Cleanup on exit
	defer c.Close()

	in := attach.Stdin()
	if in != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				var msg = make([]byte, 512)
				n, err := in.Read(msg)
				if err != nil {
					// Handle errors
					return
				}
				if n > 0 { // Only call WriteMessage if there is data to send
					if err = c.WriteMessage(websocket.BinaryMessage, msg[:n]); err != nil {
						logger.Errorf("an error has occurred while trying to write message")
						return
					}
				}
			}
		}()
	}
	if err != nil {
		return err
	}

	if out != nil {
		for {
			select {
			case <-ctx.Done():
				break
			default:
			}

			_, cr, err := c.NextReader()
			if err != nil {
				// Handle errors
				break
			}
			if _, err := io.Copy(out, cr); err != nil {
				logger.Errorf("an error has occurred while trying to copy message")
				break
			}
		}
	}
	if err != nil {
		return err
	}

	return ctx.Err()
}

// GetPodStatus returns the status of a pod by name that is running inside ACI
// returns nil if a pod by that name is not found.
func (p *ACIProvider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	ctx, span := trace.StartSpan(ctx, "aci.GetPodStatus")
	defer span.End()
	ctx = addAzureAttributes(ctx, span, p)

	cg, err := p.azClientsAPIs.GetContainerGroupInfo(ctx, p.resourceGroup, namespace, name, p.nodeName)
	if err != nil {
		return nil, err
	}

	err = validation.ValidateContainerGroup(cg)
	if err != nil {
		return nil, err
	}
	return p.getPodStatusFromContainerGroup(cg)
}

// GetPods returns a list of all pods known to be running within ACI.
func (p *ACIProvider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "aci.GetPods")
	defer span.End()
	ctx = addAzureAttributes(ctx, span, p)

	cgs, err := p.azClientsAPIs.GetContainerGroupListResult(ctx, p.resourceGroup)
	if err != nil {
		return nil, err
	}

	if cgs == nil {
		log.G(ctx).Infof("no container groups found for resource group %s", p.resourceGroup)
		return nil, nil
	}
	pods := make([]*v1.Pod, 0, len(*cgs))

	for cgIndex := range *cgs {
		validation.ValidateContainerGroup(&(*cgs)[cgIndex])
		if err != nil {
			return nil, err
		}

		if (*cgs)[cgIndex].Tags["NodeName"] != &p.nodeName {
			continue
		}

		pod, err := p.containerGroupToPod(&(*cgs)[cgIndex])
		if err != nil {
			log.G(ctx).WithFields(log.Fields{
				"name": (*cgs)[cgIndex].Name,
				"id":   (*cgs)[cgIndex].ID,
			}).WithError(err).Errorf("error converting container group %s to pod", (*cgs)[cgIndex].Name)

			continue
		}
		pods = append(pods, pod)
	}

	return pods, nil
}

// NotifyPods instructs the notifier to call the passed in function when
// the pod status changes.
// The provided pointer to a Pod is guaranteed to be used in a read-only
// fashion.
func (p *ACIProvider) NotifyPods(ctx context.Context, notifierCb func(*v1.Pod)) {
	ctx, span := trace.StartSpan(ctx, "ACIProvider.NotifyPods")
	defer span.End()

	// Capture the notifier to be used for communicating updates to VK
	p.tracker = &PodsTracker{
		rm:       p.resourceManager,
		updateCb: notifierCb,
		handler:  p,
	}

	go p.tracker.StartTracking(ctx)
}

// ListActivePods interface impl.
func (p *ACIProvider) ListActivePods(ctx context.Context) ([]PodIdentifier, error) {
	ctx, span := trace.StartSpan(ctx, "ACIProvider.ListActivePods")
	defer span.End()

	providerPods, err := p.GetPods(ctx)
	if err != nil {
		return nil, err
	}
	podsIdentifiers := make([]PodIdentifier, 0, len(providerPods))

	for _, pod := range providerPods {
		podsIdentifiers = append(
			podsIdentifiers,
			PodIdentifier{
				namespace: pod.Namespace,
				name:      pod.Name,
			})
	}

	return podsIdentifiers, nil
}

// FetchPodStatus interface impl
func (p *ACIProvider) FetchPodStatus(ctx context.Context, ns, name string) (*v1.PodStatus, error) {
	ctx, span := trace.StartSpan(ctx, "ACIProvider.FetchPodStatus")
	defer span.End()

	return p.GetPodStatus(ctx, ns, name)
}

// CleanupPod interface impl
func (p *ACIProvider) CleanupPod(ctx context.Context, ns, name string) error {
	ctx, span := trace.StartSpan(ctx, "ACIProvider.CleanupPod")
	defer span.End()

	return p.deleteContainerGroup(ctx, ns, name)
}

// implement NodeProvider

// Ping checks if the node is still active/ready.
func (p *ACIProvider) Ping(ctx context.Context) error {
	return nil
}

func (p *ACIProvider) getImagePullSecrets(pod *v1.Pod) (*[]azaci.ImageRegistryCredential, error) {
	ips := make([]azaci.ImageRegistryCredential, 0, len(pod.Spec.ImagePullSecrets))
	for _, ref := range pod.Spec.ImagePullSecrets {
		secret, err := p.resourceManager.GetSecret(ref.Name, pod.Namespace)
		if err != nil {
			return &ips, err
		}
		if secret == nil {
			return nil, fmt.Errorf("error getting image pull secret")
		}
		switch secret.Type {
		case v1.SecretTypeDockercfg:
			ips, err = readDockerCfgSecret(secret, ips)
		case v1.SecretTypeDockerConfigJson:
			ips, err = readDockerConfigJSONSecret(secret, ips)
		default:
			return nil, fmt.Errorf("image pull secret type is not one of kubernetes.io/dockercfg or kubernetes.io/dockerconfigjson")
		}

		if err != nil {
			return &ips, err
		}

	}
	return &ips, nil
}

func makeRegistryCredential(server string, authConfig AuthConfig) (*azaci.ImageRegistryCredential, error) {
	username := authConfig.Username
	password := authConfig.Password

	if username == "" {
		if authConfig.Auth == "" {
			return nil, fmt.Errorf("no username present in auth config for server: %s", server)
		}

		decoded, err := base64.StdEncoding.DecodeString(authConfig.Auth)
		if err != nil {
			return nil, fmt.Errorf("error decoding the auth for server: %s Error: %v", server, err)
		}

		parts := strings.Split(string(decoded), ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed auth for server: %s", server)
		}

		username = parts[0]
		password = parts[1]
	}

	cred := azaci.ImageRegistryCredential{
		Server:   &server,
		Username: &username,
		Password: &password,
	}

	return &cred, nil
}

func makeRegistryCredentialFromDockerConfig(server string, configEntry DockerConfigEntry) (*azaci.ImageRegistryCredential, error) {
	if configEntry.Username == "" {
		return nil, fmt.Errorf("no username present in auth config for server: %s", server)
	}

	cred := azaci.ImageRegistryCredential{
		Server:   &server,
		Username: &configEntry.Username,
		Password: &configEntry.Password,
	}

	return &cred, nil
}

func readDockerCfgSecret(secret *v1.Secret, ips []azaci.ImageRegistryCredential) ([]azaci.ImageRegistryCredential, error) {
	var err error
	var authConfigs map[string]AuthConfig
	repoData, ok := secret.Data[v1.DockerConfigKey]

	if !ok {
		return ips, fmt.Errorf("no dockercfg present in secret")
	}

	err = json.Unmarshal(repoData, &authConfigs)
	if err != nil {
		return ips, err
	}

	for server := range authConfigs {
		cred, err := makeRegistryCredential(server, authConfigs[server])
		if err != nil {
			return ips, err
		}

		ips = append(ips, *cred)
	}

	return ips, err
}

func readDockerConfigJSONSecret(secret *v1.Secret, ips []azaci.ImageRegistryCredential) ([]azaci.ImageRegistryCredential, error) {
	var err error
	repoData, ok := secret.Data[v1.DockerConfigJsonKey]

	if !ok {
		return ips, fmt.Errorf("no dockerconfigjson present in secret")
	}

	// Will use K8s config models to handle marshaling (including auth field handling).
	var cfgJson DockerConfigJSON

	err = json.Unmarshal(repoData, &cfgJson)
	if err != nil {
		return ips, err
	}

	auths := cfgJson.Auths
	if len(cfgJson.Auths) == 0 {
		return ips, fmt.Errorf("malformed dockerconfigjson in secret")
	}

	for server := range auths {
		cred, err := makeRegistryCredentialFromDockerConfig(server, auths[server])
		if err != nil {
			return ips, err
		}

		ips = append(ips, *cred)
	}

	return ips, err
}

//verify if Container is properly declared for the use on ACI
func (p *ACIProvider) verifyContainer(container *v1.Container) error {
	if len(container.Command) == 0 && len(container.Args) > 0 {
		return errdefs.InvalidInput("ACI does not support providing args without specifying the command. Please supply both command and args to the pod spec.")
	}
	return nil
}

//this method is used for both initConainers and containers
func (p *ACIProvider) getCommand(container *v1.Container) *[]string {
	command := append(container.Command, container.Args...)
	return &command
}

//get VolumeMounts declared on Container as []aci.VolumeMount
func (p *ACIProvider) getVolumeMounts(container *v1.Container) *[]azaci.VolumeMount {
	volumeMounts := make([]azaci.VolumeMount, 0, len(container.VolumeMounts))
	for i := range container.VolumeMounts {
		volumeMounts = append(volumeMounts, azaci.VolumeMount{
			Name:      &container.VolumeMounts[i].Name,
			MountPath: &container.VolumeMounts[i].MountPath,
			ReadOnly:  &container.VolumeMounts[i].ReadOnly,
		})
	}
	return &volumeMounts
}

//get EnvironmentVariables declared on Container as []aci.EnvironmentVariable
func (p *ACIProvider) getEnvironmentVariables(container *v1.Container) *[]azaci.EnvironmentVariable {
	environmentVariable := make([]azaci.EnvironmentVariable, 0, len(container.Env))
	for i := range container.Env {
		if container.Env[i].Value != "" {
			envVar := getACIEnvVar(container.Env[i])
			environmentVariable = append(environmentVariable, envVar)
		}
	}
	return &environmentVariable
}

//get InitContainers defined in Pod as []aci.InitContainerDefinition
func (p *ACIProvider) getInitContainers(ctx context.Context, pod *v1.Pod) ([]azaci.InitContainerDefinition, error) {
	initContainers := make([]azaci.InitContainerDefinition, 0, len(pod.Spec.InitContainers))
	for i, initContainer := range pod.Spec.InitContainers {
		err := p.verifyContainer(&initContainer)
		if err != nil {
			log.G(ctx).Errorf("couldn't verify container %v", err)
			return nil, err
		}

		if initContainer.Ports != nil {
			log.G(ctx).Errorf("azure container instances initcontainers do not support ports")
			return nil, errdefs.InvalidInput("azure container instances initContainers do not support ports")
		}
		if initContainer.Resources.Requests != nil {
			log.G(ctx).Errorf("azure container instances initcontainers do not support resources requests")
			return nil, errdefs.InvalidInput("azure container instances initContainers do not support resources requests")
		}
		if initContainer.Resources.Limits != nil {
			log.G(ctx).Errorf("azure container instances initcontainers do not support resources limits")
			return nil, errdefs.InvalidInput("azure container instances initContainers do not support resources limits")
		}
		if initContainer.LivenessProbe != nil {
			log.G(ctx).Errorf("azure container instances initcontainers do not support livenessProbe")
			return nil, errdefs.InvalidInput("azure container instances initContainers do not support livenessProbe")
		}
		if initContainer.ReadinessProbe != nil {
			log.G(ctx).Errorf("azure container instances initcontainers do not support readinessProbe")
			return nil, errdefs.InvalidInput("azure container instances initContainers do not support readinessProbe")
		}

		newInitContainer := azaci.InitContainerDefinition{
			Name: &pod.Spec.InitContainers[i].Name,
			InitContainerPropertiesDefinition: &azaci.InitContainerPropertiesDefinition {
				Image: &pod.Spec.InitContainers[i].Image,
				Command: p.getCommand(&pod.Spec.InitContainers[i]),
				VolumeMounts: p.getVolumeMounts(&pod.Spec.InitContainers[i]),
				EnvironmentVariables: p.getEnvironmentVariables(&pod.Spec.InitContainers[i]),
			},
		}

		initContainers = append(initContainers, newInitContainer)
	}
	return initContainers, nil
}

func (p *ACIProvider) getContainers(pod *v1.Pod) (*[]azaci.Container, error) {
	containers := make([]azaci.Container, 0, len(pod.Spec.Containers))

	podContainers := pod.Spec.Containers
	for c := range podContainers {

		if len(podContainers[c].Command) == 0 && len(podContainers[c].Args) > 0 {
			return nil, errdefs.InvalidInput("ACI does not support providing args without specifying the command. Please supply both command and args to the pod spec.")
		}
		cmd := append(podContainers[c].Command, podContainers[c].Args...)
		ports := make([]azaci.ContainerPort, 0, len(podContainers[c].Ports))
		aciContainer := azaci.Container{
			Name: &podContainers[c].Name,
			ContainerProperties: &azaci.ContainerProperties{
				Image:   &podContainers[c].Image,
				Command: &cmd,
				Ports:   &ports,
			},
		}

		for i := range podContainers[c].Ports {
			containerPorts := aciContainer.Ports
			containerPortsList := append(*containerPorts, azaci.ContainerPort{
				Port:     &podContainers[c].Ports[i].ContainerPort,
				Protocol: getProtocol(podContainers[c].Ports[i].Protocol),
			})
			aciContainer.Ports = &containerPortsList
		}

		volMount := make([]azaci.VolumeMount, 0, len(podContainers[c].VolumeMounts))
		aciContainer.VolumeMounts = &volMount
		for v := range podContainers[c].VolumeMounts {
			vol := aciContainer.VolumeMounts
			volList := append(*vol, azaci.VolumeMount{
				Name:      &podContainers[c].VolumeMounts[v].Name,
				MountPath: &podContainers[c].VolumeMounts[v].MountPath,
				ReadOnly:  &podContainers[c].VolumeMounts[v].ReadOnly,
			})
			aciContainer.VolumeMounts = &volList
		}

		initEnv := make([]azaci.EnvironmentVariable, 0, len(podContainers[c].Env))
		aciContainer.EnvironmentVariables = &initEnv
		for _, e := range podContainers[c].Env {
			env := aciContainer.EnvironmentVariables
			if e.Value != "" {
				envVar := getACIEnvVar(e)
				envList := append(*env, envVar)
				aciContainer.EnvironmentVariables = &envList
			}
		}

		// NOTE(robbiezhang): ACI CPU request must be times of 10m
		cpuRequest := 1.00
		if _, ok := podContainers[c].Resources.Requests[v1.ResourceCPU]; ok {
			cpuRequest = float64(podContainers[c].Resources.Requests.Cpu().MilliValue()/10.00) / 100.00
			if cpuRequest < 0.01 {
				cpuRequest = 0.01
			}
		}

		// NOTE(robbiezhang): ACI memory request must be times of 0.1 GB
		memoryRequest := 1.50
		if _, ok := podContainers[c].Resources.Requests[v1.ResourceMemory]; ok {
			memoryRequest = float64(podContainers[c].Resources.Requests.Memory().Value()/100000000.00) / 10.00
			if memoryRequest < 0.10 {
				memoryRequest = 0.10
			}
		}

		aciContainer.Resources = &azaci.ResourceRequirements{
			Requests: &azaci.ResourceRequests{
				CPU:        &cpuRequest,
				MemoryInGB: &memoryRequest,
			},
		}

		if podContainers[c].Resources.Limits != nil {
			cpuLimit := cpuRequest
			if _, ok := podContainers[c].Resources.Limits[v1.ResourceCPU]; ok {
				cpuLimit = float64(podContainers[c].Resources.Limits.Cpu().MilliValue()) / 1000.00
			}

			// NOTE(jahstreet): ACI memory limit must be times of 0.1 GB
			memoryLimit := memoryRequest
			if _, ok := podContainers[c].Resources.Limits[v1.ResourceMemory]; ok {
				memoryLimit = float64(podContainers[c].Resources.Limits.Memory().Value()/100000000.00) / 10.00
			}
			aciContainer.Resources.Limits = &azaci.ResourceLimits{
				CPU:        &cpuLimit,
				MemoryInGB: &memoryLimit,
			}

			if gpu, ok := podContainers[c].Resources.Limits[gpuResourceName]; ok {
				sku, err := p.getGPUSKU(pod)
				if err != nil {
					return nil, err
				}

				if gpu.Value() == 0 {
					return nil, errors.New("GPU must be a integer number")
				}

				count := int32(gpu.Value())

				gpuResource := &azaci.GpuResource{
					Count: &count,
					Sku:   azaci.GpuSku(sku),
				}

				aciContainer.Resources.Requests.Gpu = gpuResource
				aciContainer.Resources.Limits.Gpu = gpuResource
			}
		}

		if podContainers[c].LivenessProbe != nil {
			probe, err := getProbe(podContainers[c].LivenessProbe, podContainers[c].Ports)
			if err != nil {
				return nil, err
			}
			aciContainer.LivenessProbe = probe
		}

		if podContainers[c].ReadinessProbe != nil {
			probe, err := getProbe(podContainers[c].ReadinessProbe, podContainers[c].Ports)
			if err != nil {
				return nil, err
			}
			aciContainer.ReadinessProbe = probe
		}

		containers = append(containers, aciContainer)
	}
	return &containers, nil
}

func (p *ACIProvider) getGPUSKU(pod *v1.Pod) (azaci.GpuSku, error) {
	if len(p.gpuSKUs) == 0 {
		return "", fmt.Errorf("the pod requires GPU resource, but ACI doesn't provide GPU enabled container group in region %s", p.region)
	}

	if desiredSKU, ok := pod.Annotations[gpuTypeAnnotation]; ok {
		for _, supportedSKU := range p.gpuSKUs {
			if strings.EqualFold(desiredSKU, string(supportedSKU)) {
				return supportedSKU, nil
			}
		}

		return "", fmt.Errorf("the pod requires GPU SKU %s, but ACI only supports SKUs %v in region %s", desiredSKU, p.region, p.gpuSKUs)
	}

	return p.gpuSKUs[0], nil
}

func getProbe(probe *v1.Probe, ports []v1.ContainerPort) (*azaci.ContainerProbe, error) {

	if probe.Handler.Exec != nil && probe.Handler.HTTPGet != nil {
		return nil, fmt.Errorf("probe may not specify more than one of \"exec\" and \"httpGet\"")
	}

	if probe.Handler.Exec == nil && probe.Handler.HTTPGet == nil {
		return nil, fmt.Errorf("probe must specify one of \"exec\" and \"httpGet\"")
	}

	// Probes have can have an Exec or HTTP Get Handler.
	// Create those if they exist, then add to the
	// ContainerProbe struct
	var exec *azaci.ContainerExec
	if probe.Handler.Exec != nil {
		exec = &azaci.ContainerExec{
			Command: &(probe.Handler.Exec.Command),
		}
	}

	var httpGET *azaci.ContainerHTTPGet
	if probe.Handler.HTTPGet != nil {
		var portValue int32
		port := probe.Handler.HTTPGet.Port
		switch port.Type {
		case intstr.Int:
			portValue = int32(port.IntValue())
		case intstr.String:
			portName := port.String()
			for _, p := range ports {
				if portName == p.Name {
					portValue = p.ContainerPort
					break
				}
			}
			if portValue == 0 {
				return nil, fmt.Errorf("unable to find named port: %s", portName)
			}
		}

		httpGET = &azaci.ContainerHTTPGet{
			Port:   &portValue,
			Path:   &probe.Handler.HTTPGet.Path,
			Scheme: azaci.Scheme(probe.Handler.HTTPGet.Scheme),
		}
	}

	return &azaci.ContainerProbe{
		Exec:                exec,
		HTTPGet:             httpGET,
		InitialDelaySeconds: &probe.InitialDelaySeconds,
		FailureThreshold:    &probe.FailureThreshold,
		SuccessThreshold:    &probe.SuccessThreshold,
		TimeoutSeconds:      &probe.TimeoutSeconds,
		PeriodSeconds:       &probe.PeriodSeconds,
	}, nil
}

// Filters service account secret volume for Windows.
// Service account secret volume gets automatically turned on if not specified otherwise.
// ACI doesn't support secret volume for Windows, so we need to filter it.
func filterWindowsServiceAccountSecretVolume(ctx context.Context, osType string, cgw *client2.ContainerGroupWrapper) {
	if strings.EqualFold(osType, "Windows") {
		serviceAccountSecretVolumeName := make(map[string]bool)

		for index, container := range *cgw.ContainerGroupPropertiesWrapper.ContainerGroupProperties.Containers {
			volumeMounts := make([]azaci.VolumeMount, 0, len(*container.VolumeMounts))
			for _, volumeMount := range *container.VolumeMounts {
				if !strings.EqualFold(serviceAccountSecretMountPath, *volumeMount.MountPath) {
					volumeMounts = append(volumeMounts, volumeMount)
				} else {
					serviceAccountSecretVolumeName[*volumeMount.Name] = true
				}
			}
			(*cgw.ContainerGroupPropertiesWrapper.ContainerGroupProperties.Containers)[index].VolumeMounts = &volumeMounts
		}

		if len(serviceAccountSecretVolumeName) == 0 {
			return
		}

		l := log.G(ctx).WithField("containerGroup", cgw.Name)
		l.Infof("Ignoring service account secret volumes '%v' for Windows", reflect.ValueOf(serviceAccountSecretVolumeName).MapKeys())

		volumes := make([]azaci.Volume, 0, len(*cgw.ContainerGroupPropertiesWrapper.ContainerGroupProperties.Volumes))
		for _, volume := range *cgw.ContainerGroupPropertiesWrapper.ContainerGroupProperties.Volumes {
			if _, ok := serviceAccountSecretVolumeName[*volume.Name]; !ok {
				volumes = append(volumes, volume)
			}
		}

		cgw.ContainerGroupPropertiesWrapper.ContainerGroupProperties.Volumes = &volumes
	}
}

func getACIEnvVar(e v1.EnvVar) azaci.EnvironmentVariable {
	var envVar azaci.EnvironmentVariable
	// If the variable is a secret, use SecureValue
	if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
		envVar = azaci.EnvironmentVariable{
			Name:        &e.Name,
			SecureValue: &e.Value,
		}
	} else {
		envVar = azaci.EnvironmentVariable{
			Name:  &e.Name,
			Value: &e.Value,
		}
	}
	return envVar
}
