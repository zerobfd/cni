package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/containernetworking/plugins/pkg/ns"
	"io/ioutil"
	"istio.io/cni/pkg/istioproxyagent/api"
	"k8s.io/api/core/v1"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/apis/cri"
	criapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/remote"
	"k8s.io/kubernetes/third_party/forked/golang/expansion"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	containerName = "istio-proxy"
)

type CRIRuntime struct {
	kubeClient     *KubernetesClient
	runtimeService cri.RuntimeService
	imageService   cri.ImageManagerService
	httpClient     http.Client
}

func NewCRIRuntime(kubeclient *KubernetesClient) (*CRIRuntime, error) {
	runtimeService, err := remote.NewRemoteRuntimeService(getRemoteRuntimeEndpoint(), 2*time.Minute)
	if err != nil {
		return nil, err
	}

	imageService, err := remote.NewRemoteImageService(getRemoteImageEndpoint(), 2*time.Minute)
	if err != nil {
		return nil, err
	}

	return &CRIRuntime{
		kubeClient:     kubeclient,
		runtimeService: runtimeService,
		imageService:   imageService,
		httpClient:     http.Client{},
	}, nil
}

func (p *CRIRuntime) StartProxy(podSandboxID string, pod *v1.Pod, sidecar *v1.Container) error {

	err := p.pullImageIfNecessary(sidecar.Image)
	if err != nil {
		return fmt.Errorf("Could not pull image %s: %v", sidecar.Image, err)
	}

	status, err := p.runtimeService.PodSandboxStatus(podSandboxID)
	if err != nil {
		return fmt.Errorf("Error getting pod sandbox status: %v", err)
	}

	klog.Info("Creating volumes")
	mounts, err := p.createVolumeMounts(pod, sidecar)
	if err != nil {
		return fmt.Errorf("Error creating volumes: %v", err)
	}

	envs, err := convertEnvs(pod, sidecar.Env, sidecar.EnvFrom)
	if err != nil {
		return fmt.Errorf("Error converting env vars: %v", err)
	}

	expandVars(sidecar.Command, envs)
	expandVars(sidecar.Args, envs)

	containerConfig := criapi.ContainerConfig{
		Metadata: &criapi.ContainerMetadata{
			Name: containerName,
		},
		Image: &criapi.ImageSpec{
			Image: sidecar.Image,
		},
		Command: sidecar.Command,
		Args:    sidecar.Args,
		Linux: &criapi.LinuxContainerConfig{
			Resources: &criapi.LinuxContainerResources{
				// TODO
			},
			SecurityContext: &criapi.LinuxContainerSecurityContext{
				RunAsUser:          &criapi.Int64Value{*sidecar.SecurityContext.RunAsUser},
				SupplementalGroups: []int64{0},
				Privileged:         *sidecar.SecurityContext.Privileged,
				ReadonlyRootfs:     *sidecar.SecurityContext.ReadOnlyRootFilesystem,
			},
		},
		Windows: &criapi.WindowsContainerConfig{
			Resources: &criapi.WindowsContainerResources{
				// TODO
			},
			SecurityContext: &criapi.WindowsContainerSecurityContext{
				RunAsUsername: "NotImplemented", // TODO
			},
		},
		Envs:   envs,
		Mounts: mounts,
		Labels: map[string]string{
			"io.kubernetes.container.name": containerName,
			"io.kubernetes.pod.name":       pod.Name,
			"io.kubernetes.pod.namespace":  pod.Namespace,
			"io.kubernetes.pod.uid":        string(pod.UID),
		},
		Annotations: map[string]string{
			"io.kubernetes.container.terminationMessagePath":   "/dev/termination-log",
			"io.kubernetes.container.terminationMessagePolicy": "File",
			"io.kubernetes.container.hash":                     "0", // TODO
			"io.kubernetes.container.restartCount":             "0", // TODO
		},
	}

	klog.Infof("containerConfig: %v", toDebugJSON(containerConfig))

	podSandboxConfig := criapi.PodSandboxConfig{
		Metadata: status.GetMetadata(),
	}

	klog.Infof("Creating proxy sidecar container for pod %s", pod.Name)
	containerID, err := p.runtimeService.CreateContainer(podSandboxID, &containerConfig, &podSandboxConfig)
	if err != nil {
		return fmt.Errorf("Error creating sidecar container: %v", err)
	}
	klog.Infof("Created proxy sidecar container: %s", containerID)

	err = p.runtimeService.StartContainer(containerID)
	if err != nil {
		return fmt.Errorf("Error starting sidecar container: %v", err)
	}
	klog.Infof("Started proxy sidecar container: %s", containerID)

	return nil
}

func expandVars(strings []string, envVars []*criapi.KeyValue) {
	mappingFunc := expansion.MappingFuncFor(EnvVarsToMap(envVars))

	for i, s := range strings {
		strings[i] = expansion.Expand(s, mappingFunc)
	}
}

func EnvVarsToMap(envs []*criapi.KeyValue) map[string]string {
	result := map[string]string{}
	for _, env := range envs {
		result[env.Key] = env.Value
	}

	return result
}

func sidecarTemplateVersionHash(in string) string {
	hash := sha256.Sum256([]byte(in))
	return hex.EncodeToString(hash[:])
}

func convertEnvs(pod *v1.Pod, env []v1.EnvVar, envFromSources []v1.EnvFromSource) ([]*criapi.KeyValue, error) {
	if len(envFromSources) > 0 {
		return nil, fmt.Errorf("EnvFrom not supported")
	}

	r := []*criapi.KeyValue{}

	tmpEnv := make(map[string]string)
	mappingFunc := expansion.MappingFuncFor(tmpEnv)

	for _, e := range env {
		value := e.Value

		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			fieldRef := e.ValueFrom.FieldRef
			switch {
			case fieldRef.FieldPath == "metadata.uid":
				value = string(pod.UID)
			case fieldRef.FieldPath == "metadata.name":
				value = pod.Name
			case fieldRef.FieldPath == "metadata.namespace":
				value = pod.Namespace
			case fieldRef.FieldPath == "status.podIP":
				value = pod.Status.PodIP
			}
		}

		value = expansion.Expand(value, mappingFunc)

		tmpEnv[e.Name] = value
		r = append(r, &criapi.KeyValue{
			Key:   e.Name,
			Value: value,
		})
	}

	return r, nil
}

func (p *CRIRuntime) pullImageIfNecessary(image string) error {
	klog.Infof("Checking if image %s is available locally", image)

	imageSpec := criapi.ImageSpec{
		Image: image,
	}
	imageStatus, err := p.imageService.ImageStatus(&imageSpec)
	if err != nil {
		return fmt.Errorf("Error getting image status: %v", err)
	}

	if imageStatus == nil {
		klog.Infof("Pulling image %s is available locally", image)
		var authConfig *criapi.AuthConfig = nil // TODO: implement image pull authentication
		imageRef, err := p.imageService.PullImage(&imageSpec, authConfig)
		if err != nil {
			return fmt.Errorf("Error pulling image: %v", err)
		}
		klog.Infof("Successfully pulled image. Image ref: %s", imageRef)
	} else {
		klog.Info("Image is available locally. No need to pull it.")
	}

	return nil
}

func getRemoteImageEndpoint() string {
	return getRemoteRuntimeEndpoint()
}

func getRemoteRuntimeEndpoint() string {
	if runtime.GOOS == "linux" {
		return "unix:///var/run/dockershim.sock"
	} else if runtime.GOOS == "windows" {
		return "npipe:////./pipe/dockershim"
	}
	return ""
}

func (p *CRIRuntime) StopProxy(request *api.StopRequest) error {

	containerID, err := p.findProxyContainerID(request.PodSandboxID)
	if err != nil {
		return err
	}

	err = p.runtimeService.StopContainer(containerID, 30000) // TODO: make timeout configurable
	if err != nil {
		return err
	}

	return nil
}

func (p *CRIRuntime) IsReady(request *api.ReadinessRequest) (bool, error) {
	ready := false

	netNS := strings.Replace(request.NetNS, "/proc/", "/hostproc/", 1) // we're running in a container; host's /proc/ is mapped to /hostproc/

	err := ns.WithNetNSPath(netNS, func(hostNS ns.NetNS) error {
		//url := "http://" + request.PodIP + ":" + "15000" + "/server_info" // TODO: make port & path configurable
		url := "http://" + "localhost" + ":" + "15000" + "/server_info" // TODO: make port & path configurable
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		response, err := p.httpClient.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()

		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusBadRequest {
			klog.Infof("Readiness probe succeeded for %s", request.PodName)
			ready = true
			return nil
		}
		klog.Infof("Readiness probe failed for %s (%s): %v %s", request.PodName, url, response.StatusCode, response.Status)
		return nil
	})

	return ready, err
}

func (p *CRIRuntime) findProxyContainerID(podSandboxId string) (string, error) {
	containers, err := p.runtimeService.ListContainers(&criapi.ContainerFilter{
		PodSandboxId: podSandboxId,
	})
	if err != nil {
		return "", err
	}

	container, err := p.findContainerByName(containerName, containers)
	if err != nil {
		return "", err
	}

	return container.Id, nil
}

func (p *CRIRuntime) findContainerByName(name string, containers []*criapi.Container) (*criapi.Container, error) {
	for _, c := range containers {
		if c.Metadata.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("Could not find container %q in list of containers", containerName)
}

func (p *CRIRuntime) createVolumeMounts(pod *v1.Pod, sidecar *v1.Container) ([]*criapi.Mount, error) {
	random := rand.New(rand.NewSource(time.Now().UnixNano()))
	dir := "/tmp/istio-proxy-volumes-" + strconv.Itoa(random.Int())
	certsDir := dir + "/certs"
	err := os.MkdirAll(certsDir, os.ModePerm)
	if err != nil {
		return nil, err
	}

	confDir := dir + "/conf"
	err = os.Mkdir(confDir, os.ModePerm)
	if err != nil {
		return nil, err
	}

	// ensure the conf dir is world writable (might not be if umask is set)
	err = os.Chmod(confDir, 0777)
	if err != nil {
		return nil, err
	}

	klog.Infof("Geting Secret %s in namespace %s", "istio.default", pod.Namespace)
	secretData, k8sErr := p.kubeClient.getSecret("istio.default", pod.Namespace) // TODO: get secret name
	if k8sErr != nil {
		return nil, fmt.Errorf("Error geting Secret data %v", k8sErr)
	}

	klog.Infof("Writing secret data to %s", certsDir)
	err = writeSecret(certsDir, secretData)
	if err != nil {
		return nil, fmt.Errorf("Error writing secret data: %v", err)
	}

	return []*criapi.Mount{
		{
			ContainerPath: "/etc/istio/proxy/",
			HostPath:      confDir,
			Readonly:      false,
		},
		{
			ContainerPath: "/etc/certs/",
			HostPath:      certsDir,
			Readonly:      true,
		},
	}, nil
}

func writeSecret(dir string, secretData map[string][]byte) error {
	for k, v := range secretData {
		err := ioutil.WriteFile(dir+"/"+k, v, os.ModePerm)
		if err != nil {
			return err
		}
	}
	return nil
}
