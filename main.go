package main

import (
	"bytes"
	"context"
	_ "embed"
	json2 "encoding/json"
	"flag"
	"fmt"
	"io"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/homedir"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

type ContainerInfo struct {
	podName       string
	containerName string
	shell         string
	testable      bool
}

//utils                                   []string = []string{"stat /usr/bin/find", "stat /bin/cat", "stat /bin/ps", "stat /bin/grep"}

var (
	debug                 bool
	kubeconfig            *string
	namespace             *string
	config                *rest.Config
	clientset             *kubernetes.Clientset
	utils                 []string = []string{"stat /usr/bin/find", "stat /bin/cat", "stat /bin/grep"}
	targetContainers      []ContainerInfo
	nontestableContainers []ContainerInfo
	logBuffer             bytes.Buffer
	pod                   *string
	container             *string
)

//go:embed data/lse.sh
var lse []byte

func exec(clientset *kubernetes.Clientset, config *rest.Config, namespace string, podName string, containerName string, cmd string, stdin io.Reader, stdout io.Writer, stderr io.Writer, tty bool) error {
	req := clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   strings.Fields(cmd),
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
			TTY:       tty,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[-] Execution failed with error code %d\n", err)
		}
		return err
	}

	err = executor.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    false,
	})

	//fmt.Println(stdout)

	return err
}

// checkShellsInContainer checks for the presence of specified shells in the given container of a pod.
func getShellInContainer(clientset *kubernetes.Clientset, config *rest.Config, namespace string, podName string, containerName string) (string, error) {

	var stdout, stderr bytes.Buffer
	err := exec(clientset, config, namespace, podName, containerName, "sh --version", nil, &stdout, &stderr, false)

	if err == nil {
		return "sh", nil
	}

	err = exec(clientset, config, namespace, podName, containerName, "bash --version", nil, &stdout, &stderr, false)
	if err == nil {
		return "bash", nil
	}

	return "", err
}

func checkUtilInContainer(clientset *kubernetes.Clientset, config *rest.Config, namespace, podName, containerName string, util string) (bool, error) {
	var stdout, stderr bytes.Buffer
	err := exec(clientset, config, namespace, podName, containerName, util, nil, &stdout, &stderr, false)
	return err == nil, err
}

type UtilsStatus map[string]map[string]map[string]bool

var status UtilsStatus

func checkUtils(clientset *kubernetes.Clientset, config *rest.Config, podName string, containerName string, namespace string, utils []string) {
	for _, util := range utils {
		utilFound, _ := checkUtilInContainer(clientset, config, namespace, podName, containerName, util)
		status[podName][containerName][util] = utilFound
	}
}

func getPods(clientset *kubernetes.Clientset, namespace string, options metaV1.ListOptions) ([]corev1.Pod, error) {
	var pods *corev1.PodList
	pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), options)
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func getDeployments(clientset *kubernetes.Clientset, namespace string) (*v1.DeploymentList, error) {
	var deployments *v1.DeploymentList
	deployments, err := clientset.AppsV1().Deployments(namespace).List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return deployments, nil
}

// mapToLabelSelector converts a map of key-value pairs to a Kubernetes label selector string.
func mapToLabelSelector(labels map[string]string) string {
	var selectorParts []string
	for key, value := range labels {
		selectorParts = append(selectorParts, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(selectorParts, ",")
}

func getUniquePods(clientset *kubernetes.Clientset, namespace string) (int, []corev1.Pod, error) {
	var uniquePods []corev1.Pod
	var podCount int

	deployments, err := getDeployments(clientset, namespace)
	if err != nil {
		return 0, nil, err
	}

	for _, deployment := range deployments.Items {
		options := metaV1.ListOptions{LabelSelector: mapToLabelSelector(deployment.Spec.Selector.MatchLabels)}
		pods, err := getPods(clientset, namespace, options)
		if err != nil {
			continue
		}
		// we are interested only in one instance of a pod
		podCount += len(pods)
		if len(pods) > 0 {
			//uniquePods = append(uniquePods, pods...)
			uniquePods = append(uniquePods, pods[0])
		}
	}

	return podCount, uniquePods, nil
}

func verifyContainers(pods []corev1.Pod) (target []ContainerInfo, nontestable []ContainerInfo) {
	//var podChan chan ContainerInfo = make(chan ContainerInfo, 20)

	status = make(UtilsStatus)
	shell := make(map[string]map[string]string)

	if len(utils) == 0 {
		return nil, nil
	}

	//go func() {
	for _, pod := range pods {
		if pod.Status.Phase == "Running" {
			status[pod.Name] = make(map[string]map[string]bool)
			shell[pod.Name] = make(map[string]string)
			for _, container := range pod.Spec.Containers {
				status[pod.Name][container.Name] = make(map[string]bool)
				checkUtils(clientset, config, pod.Name, container.Name, *namespace, utils)
				shell[pod.Name][container.Name], _ = getShellInContainer(clientset, config, *namespace, pod.Name, container.Name)
			}
		}
	}
	//}()

	if len(status) > 0 {
		for pod, containers := range status {
			for container, utilsStatus := range containers {
				canBeTested := true
				for _, present := range utilsStatus {
					canBeTested = canBeTested && present
					canBeTested = canBeTested && shell[pod][container] != ""
				}
				if canBeTested {
					target = append(target, ContainerInfo{podName: pod, containerName: container, shell: shell[pod][container]})
				} else {
					nontestable = append(nontestable, ContainerInfo{podName: pod, containerName: container, shell: shell[pod][container]})
				}
			}
		}
	}
	if debug {
		jb, _ := json2.MarshalIndent(status, "", "    ")
		fmt.Println(string(jb))
	}
	return target, nontestable
}

func promptYN(prompt string) bool {
	var response string

	for {
		fmt.Print(prompt)
		_, err := fmt.Scanf("%s\n", &response)

		if err != nil {
			fmt.Println("Error reading input. Please try again.")
			continue
		}

		response = strings.ToUpper(strings.TrimSpace(response))

		if response == "Y" {
			return true
		} else if response == "N" {
			return false
		} else {
			fmt.Println("Invalid input. Please enter 'Y' or 'N'.")
		}
	}
}

func init() {
	var err error
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	namespace = flag.String("namespace", "default", "CNF namespace")
	pod = flag.String("pod", "", "Pod name")
	container = flag.String("container", "", "Container name")
	flag.BoolVar(&debug, "debug", false, "turn on debugging mode")
	flag.Parse()

	config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
}

func main() {
	fmt.Println("[+] Started")

	//pods, err := getPods(clientset, *namespace, metaV1.ListOptions{})
	podCount, pods, err := getUniquePods(clientset, *namespace)
	if err != nil {
		fmt.Println(err)
		os.Exit(0)
	}

	if len(pods) == 0 {
		fmt.Printf("[-] No pods found in namespace %q\n", *namespace)
		os.Exit(0)
	}
	fmt.Printf("[+] Found %d unique pods out of %d deployments related pods in %s namespace\n", len(pods), podCount, *namespace)
	fmt.Println("[*] Identifying testable containers")
	targetContainers, nontestableContainers = verifyContainers(pods)
	fmt.Printf("[+] Found %d containers in %s namespace\n", len(targetContainers)+len(nontestableContainers), *namespace)
	if len(targetContainers) > 0 {
		fmt.Println("[+] Following containers can be tested:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
		for _, list := range targetContainers {
			fmt.Fprintf(w, "%s\t%s\n", list.podName, list.containerName)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
	} else {
		fmt.Println("[-] Did not find any testable containers")
	}

	if len(nontestableContainers) > 0 {
		fmt.Println("[-] Following containers cannot be tested:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
		for _, container := range nontestableContainers {
			fmt.Fprintf(w, "%s\t%s\n", container.podName, container.containerName)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
	}

	if promptYN("\nDo you wish to proceed with testing? (Y/N): ") {
		fmt.Println("Proceeding with testing...")
	} else {
		fmt.Println("Action cancelled.")
		os.Exit(1)
	}

	if len(targetContainers) > 0 {
		lsetmp := bytes.Replace(lse, []byte("\r\n"), []byte("\n"), -1)
		lsetmp = bytes.Replace(lsetmp, []byte("\r"), []byte(""), -1)
		tested := make(map[string]int)
		for _, container := range targetContainers {
			var stdout, stderr bytes.Buffer

			if _, done := tested[container.containerName]; done {
				fmt.Printf("[*] Skipping %s/%s container since %s contaner has already been tested\n", container.podName, container.containerName, container.containerName)
				continue
			}

			lsescript := bytes.NewBuffer(lsetmp)

			fmt.Printf("[+] Testing %s/%s container\n", container.podName, container.containerName)
			err := exec(clientset, config, *namespace, container.podName, container.containerName, container.shell, lsescript, &stdout, &stderr, false)
			if err == nil {
				fmt.Println(stdout.String())
				tested[container.containerName]++
			} else {
				fmt.Printf("[-] Error encounter: %s\n", stderr.String())
			}
		}
	}
}
