package main

import (
	"bytes"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"github.com/robert-nix/ansihtml"
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
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

var (
	htmlHeader = `
<?xml version="1.0" encoding="UTF-8" ?>
<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd">
<html xmlns="http://www.w3.org/1999/xhtml">
<head>
<meta http-equiv="Content-Type" content="application/xml+xhtml; charset=UTF-8"/>
<title>stdin</title>
</head>
<body style="color:white; background-color:black">
<pre>`

	htmlFooter = `
</pre>
</body>
</html>`
)

type ContainerInfo struct {
	podName       string
	containerName string
	shell         string
	testable      bool
}

type Result struct {
	podName       string
	containerName string
	scanReport    bytes.Buffer
}

//utils                                   []string = []string{"stat /usr/bin/find", "stat /bin/cat", "stat /bin/ps", "stat /bin/grep"}

var (
	debug                 bool
	kubeconfig            *string
	namespace             *string
	format                *string
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

func checkUtilsv2(clientset *kubernetes.Clientset, config *rest.Config, podName string, containerName string, namespace string, utils []string) bool {
	var utilFound bool = true
	for _, util := range utils {
		result, _ := checkUtilInContainer(clientset, config, namespace, podName, containerName, util)
		utilFound = utilFound && result
		if result == false {
			break
		}
	}
	return utilFound
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

func getStatefulSets(clientset *kubernetes.Clientset, namespace string) (*v1.StatefulSetList, error) {
	var statefulSets *v1.StatefulSetList
	statefulSets, err := clientset.AppsV1().StatefulSets(namespace).List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return statefulSets, nil
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

	var deploymentPods map[string]int = make(map[string]int)
	deployments, err := getDeployments(clientset, namespace)
	if err != nil {
		return 0, nil, err
	}

	for _, deployment := range deployments.Items {
		// to find all pods that are part of a given deployment we need to use deployment.Spec.Selector.MatchLabels
		// from the deployment. This is essential.
		options := metaV1.ListOptions{LabelSelector: mapToLabelSelector(deployment.Spec.Selector.MatchLabels)}
		pods, err := getPods(clientset, namespace, options)
		if err != nil {
			continue
		}
		// we are interested only in one instance of a pod
		if len(pods) > 0 {
			uniquePods = append(uniquePods, pods[0])
		}
		for _, pod := range pods {
			deploymentPods[pod.Name]++
		}
	}
	fmt.Fprintf(os.Stderr, "[+] Found %d pods in %d deployments\n", len(deploymentPods), len(deployments.Items))

	var statefulSetsPods map[string]int = make(map[string]int)
	statefulSets, err := getStatefulSets(clientset, namespace)
	if err != nil {
		return 0, nil, err
	}

	for _, statefulSet := range statefulSets.Items {
		// to find all pods that are part of a given deployment we need to use statefulSet.Spec.Selector.MatchLabels
		// from the deployment. This is essential.
		options := metaV1.ListOptions{LabelSelector: mapToLabelSelector(statefulSet.Spec.Selector.MatchLabels)}
		pods, err := getPods(clientset, namespace, options)
		if err != nil {
			continue
		}
		// we are interested only in one instance of a pod
		//podCount += len(pods)
		if len(pods) > 0 {
			uniquePods = append(uniquePods, pods[0])
		}
		for _, pod := range pods {
			statefulSetsPods[pod.Name]++
		}
	}
	fmt.Fprintf(os.Stderr, "[+] Found %d pods in %d statefulsets\n", len(statefulSetsPods), len(statefulSets.Items))

	podsList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return 0, nil, err
	}
	for _, pod := range podsList.Items {
		if _, ok := deploymentPods[pod.Name]; ok {
			continue
		}
		if _, ok := statefulSetsPods[pod.Name]; ok {
			continue
		}
		uniquePods = append(uniquePods, pod)
	}

	return len(podsList.Items), uniquePods, nil
}

func getUniquePodsForDeployments(clientset *kubernetes.Clientset, namespace string) (int, []corev1.Pod, error) {
	var uniquePods []corev1.Pod
	var podCount int

	deployments, err := getDeployments(clientset, namespace)
	if err != nil {
		return 0, nil, err
	}

	for _, deployment := range deployments.Items {
		// to find all pods that are part of a given deployment we need to use deployment.Spec.Selector.MatchLabels
		// from the deployment. This is essential.
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
	var (
		podProdChan chan ContainerInfo = make(chan ContainerInfo, 20)
		conProdChan chan ContainerInfo = make(chan ContainerInfo, 100)
	)
	var (
		podWg           sync.WaitGroup
		contVerWorkerWg sync.WaitGroup
		contCollectorWg sync.WaitGroup
	)

	if len(utils) == 0 {
		return nil, nil
	}

	// these are workers that check shell and utilities
	for i := 0; i < 200; i++ {
		contVerWorkerWg.Add(1)
		go func() {
			defer contVerWorkerWg.Done()
			for container := range podProdChan {
				container.shell, _ = getShellInContainer(clientset, config, *namespace, container.podName, container.containerName)
				container.testable = checkUtilsv2(clientset, config, container.podName, container.containerName, *namespace, utils) && container.shell != ""
				conProdChan <- container
			}
		}()
	}

	// this goroutine distributes found pods through podProdChan for workers that check shell and utilities
	podWg.Add(1)
	go func() {
		defer podWg.Done()
		for _, pod := range pods {
			if pod.Status.Phase == "Running" {
				for _, container := range pod.Spec.Containers {
					podProdChan <- ContainerInfo{podName: pod.Name, containerName: container.Name}
				}
			}
		}
	}()

	// this results collector goroutine that gets verified containers from workers and puts them into two buckets (slices):
	// - bucket containing containers that will be tested with lse.sh because they have everything needed
	// - bucket with containers that lack utilities and cannot be tested with lse.sh
	contCollectorWg.Add(1)
	go func() {
		defer contCollectorWg.Done()
		for container := range conProdChan {
			switch {
			case container.testable:
				target = append(target, container)
			case !container.testable:
				nontestable = append(nontestable, container)
			}
		}
	}()

	podWg.Wait()
	close(podProdChan)
	contVerWorkerWg.Wait()
	close(conProdChan)
	contCollectorWg.Wait()

	return target, nontestable
}

func promptYN(prompt string) bool {
	var response string

	for {
		fmt.Fprint(os.Stderr, prompt)
		_, err := fmt.Scanf("%s\n", &response)

		if err != nil {
			fmt.Fprintln(os.Stderr, "Error reading input. Please try again.")
			continue
		}

		response = strings.ToUpper(strings.TrimSpace(response))

		if response == "Y" {
			return true
		} else if response == "N" {
			return false
		} else {
			fmt.Fprintln(os.Stderr, "Invalid input. Please enter 'Y' or 'N'.")
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
	format = flag.String("o", "ansi", "Output format (ansi, text, html")
	namespace = flag.String("namespace", "default", "CNF namespace")
	pod = flag.String("pod", "", "Pod name")
	container = flag.String("container", "", "Container name")
	//flag.BoolVar(&debug, "debug", false, "turn on debugging mode")
	flag.Parse()

	if _, ok := map[string]int{"ansi": 0, "text": 0, "html": 0}[*format]; !ok {
		fmt.Fprintln(os.Stderr, "Invalid value of the output format option '-o'. Valid values are ansi, text or html\n")
		flag.Usage()
		os.Exit(1)
	}

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

func saveScan(podName, containerName string, scanReport bytes.Buffer) {
	fileName := fmt.Sprintf("%s-%s-%s.%s", podName, containerName, time.Now().Format("2006-01-02-150405"), *format)
	fileName, err := filepath.Abs(fileName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "[!!] Cannot create file path for a scan report for %s/%s container%s\n", podName, containerName)
		fmt.Println(scanReport.String())
		return
	}

	var report []byte
	switch *format {
	case "html":
		report = []byte(htmlHeader)
		report = append(report, ansihtml.ConvertToHTML(scanReport.Bytes())...)
		report = append(report, []byte(htmlFooter)...)
	default:
		report = scanReport.Bytes()
	}

	err = os.WriteFile(fileName, report, 0666)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "[!!] Cannot save scan report for %s/%s container to file %s\n", podName, containerName, fileName)
		fmt.Println(scanReport.String())
		return
	}
}

func scan(pods []corev1.Pod, namespace string) {
	fmt.Fprintln(os.Stderr, "[*] Identifying testable containers")
	targetContainers, nontestableContainers = verifyContainers(pods)
	fmt.Fprintf(os.Stderr, "[+] Found %d unique containers\n", len(targetContainers)+len(nontestableContainers))

	if len(targetContainers) > 0 {
		fmt.Fprintf(os.Stderr, "[+] Following %d containers can be tested:\n", len(targetContainers))
		w := tabwriter.NewWriter(os.Stderr, 0, 0, 1, ' ', 0)
		for _, list := range targetContainers {
			fmt.Fprintf(w, "%s\t%s\n", list.podName, list.containerName)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
	} else {
		fmt.Fprintln(os.Stderr, "[-] Did not find any testable containers")
		return
	}

	if len(nontestableContainers) > 0 {
		fmt.Fprintf(os.Stderr, "[-] Following %d containers cannot be tested:\n", len(nontestableContainers))
		w := tabwriter.NewWriter(os.Stderr, 0, 0, 1, ' ', 0)
		for _, container := range nontestableContainers {
			fmt.Fprintf(w, "%s\t%s\n", container.podName, container.containerName)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
	}

	if promptYN("\nDo you wish to proceed with testing? (Y/N): ") {
		fmt.Fprintln(os.Stderr, "Proceeding with testing...")
	} else {
		fmt.Fprintln(os.Stderr, "Action cancelled.")
		os.Exit(1)
	}

	if len(targetContainers) > 0 {
		var workers int = 200

		if len(targetContainers) < 200 {
			workers = len(targetContainers)
		}

		var (
			contProdChan    chan ContainerInfo = make(chan ContainerInfo, runtime.NumCPU()*2)
			resultsProdChan chan Result        = make(chan Result, runtime.NumCPU()*2)
		)

		var (
			contFanOutWg       sync.WaitGroup
			testWorkerWg       sync.WaitGroup
			resultsCollectorWg sync.WaitGroup
		)

		// this is necessary, when cross-compiling on windows
		lsetmp := bytes.Replace(lse, []byte("\r\n"), []byte("\n"), -1)
		lsetmp = bytes.Replace(lsetmp, []byte("\r"), []byte(""), -1)

		contFanOutWg.Add(1)
		go func() {
			defer contFanOutWg.Done()
			for _, container := range targetContainers {
				contProdChan <- container
			}
		}()

		for id := 0; id < workers; id++ {
			var stdout, stderr bytes.Buffer

			testWorkerWg.Add(1)
			go func() {
				defer testWorkerWg.Done()
				for container := range contProdChan {
					lsescript := bytes.NewBuffer(lsetmp)
					shell := container.shell
					if *format == "text" {
						shell = fmt.Sprintf("%s -s -- -c", shell)
					}
					err := exec(clientset, config, namespace, container.podName, container.containerName, shell, lsescript, &stdout, &stderr, false)
					if err == nil {
						resultsProdChan <- Result{container.podName, container.containerName, stdout}
					}
				}
			}()
		}

		resultsCollectorWg.Add(1)
		go func() {
			var cnt int

			defer resultsCollectorWg.Done()
			for result := range resultsProdChan {
				saveScan(result.podName, result.containerName, result.scanReport)
				cnt++
				fmt.Fprintf(os.Stderr, "\rAnalyzed %d containers", cnt)
			}
		}()

		contFanOutWg.Wait()
		close(contProdChan)
		testWorkerWg.Wait()
		close(resultsProdChan)
		resultsCollectorWg.Wait()
	}
}

func scanPod(podName, namespace string) {
	fmt.Fprintln(os.Stderr, "[+] Started")
	fmt.Fprintln(os.Stderr, "[+] Searching for %s pod", podName)
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metaV1.GetOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "[+] Pod %s found", podName)
	scan([]corev1.Pod{*pod}, namespace)
}

func scanContainer(podName, containerName, namespace string) {
	fmt.Fprintln(os.Stderr, "[+] Started")
	fmt.Fprintln(os.Stderr, "[+] Searching for %s/%s container", podName, containerName)
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metaV1.GetOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	found := false
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, "[+] Container %s/%s not found. Aborting", podName, containerName)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "[+] Container %s/%s found", podName, containerName)
	// this is necessary, when cross-compiling on windows
	lsetmp := bytes.Replace(lse, []byte("\r\n"), []byte("\n"), -1)
	lsetmp = bytes.Replace(lsetmp, []byte("\r"), []byte(""), -1)

	lsescript := bytes.NewBuffer(lsetmp)

	shell, _ := getShellInContainer(clientset, config, namespace, podName, containerName)
	testable := checkUtilsv2(clientset, config, podName, containerName, namespace, utils) && shell != ""

	if !testable {
		fmt.Fprintf(os.Stderr, "[!!] Container %s/%s is not testable.", podName, containerName)
		os.Exit(1)
	}

	if *format == "text" {
		shell = fmt.Sprintf("%s -s -- -c", shell)
	}

	var stdout, stderr bytes.Buffer

	err = exec(clientset, config, namespace, podName, containerName, shell, lsescript, &stdout, &stderr, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

}

func scanNamespace(namespace string) {
	fmt.Fprintln(os.Stderr, "[+] Started")
	fmt.Fprintln(os.Stderr, "[+] Creating a list of unique pods")

	//pods, err := getPods(clientset, *namespace, metaV1.ListOptions{})
	podCount, pods, err := getUniquePods(clientset, namespace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if len(pods) == 0 {
		fmt.Fprintf(os.Stderr, "[-] No pods found in namespace %q\n", namespace)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[+] Found %d unique pods out of %d pods in %s namespace\n", len(pods), podCount, namespace)
	scan(pods, namespace)
}

func main() {
	switch {
	case *pod != "" && *container == "":
		scanPod(*pod, *namespace)
	case *pod != "" && *container != "":
		scanContainer(*pod, *container, *namespace)
	case *pod == "" && *container == "":
		scanNamespace(*namespace)
	default:
		flag.Usage()
	}
}
