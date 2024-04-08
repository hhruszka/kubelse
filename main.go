package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/robert-nix/ansihtml"
	"github.com/spf13/cobra"
	"io"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	exec2 "k8s.io/client-go/util/exec"
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

// utils                                   []string = []string{"stat /usr/bin/find", "stat /bin/cat", "stat /bin/ps", "stat /bin/grep"}
// App global variables
var (
	config                *rest.Config
	clientset             *kubernetes.Clientset
	utils                 []string = []string{"stat /usr/bin/find", "stat /bin/cat", "stat /bin/grep"}
	targetContainers      []ContainerInfo
	nontestableContainers []ContainerInfo
	logBuffer             chan string = make(chan string, runtime.NumCPU())
	logWg                 sync.WaitGroup
)

// CLI options variables
var (
	debug      bool
	kubeconfig string
	namespace  string
	format     string
	pod        string
	container  string
	directory  string
	quiet      bool
	list       bool
)

//go:embed data/lse.sh
var lse []byte

func log(msg string) {
	if !quiet {
		logBuffer <- msg
	}
}

func exit(code int) {
	close(logBuffer)
	logWg.Wait()

}

func exec(clientset *kubernetes.Clientset, config *rest.Config, namespace string, podName string, containerName string, cmd string, stdin io.Reader, stdout io.Writer, stderr io.Writer, tty bool) (int, error) {
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
			log(fmt.Sprintf("[-] Execution failed with error code %d\n", err))
		}
		return -1, err
	}

	err = executor.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    false,
	})
	if err != nil {
		exitError := exec2.CodeExitError{}
		if errors.As(err, &exitError) {
			return exitError.Code, exitError
		}
		return -1, err
	}

	return 0, nil
}

// checkShellsInContainer checks for the presence of specified shells in the given container of a pod.
func getShellInContainer(clientset *kubernetes.Clientset, config *rest.Config, namespace string, podName string, containerName string) (string, error) {

	var stdout, stderr bytes.Buffer
	_, err := exec(clientset, config, namespace, podName, containerName, "sh --version", nil, &stdout, &stderr, false)

	if err == nil {
		return "sh", nil
	}

	_, err = exec(clientset, config, namespace, podName, containerName, "bash --version", nil, &stdout, &stderr, false)
	if err == nil {
		return "bash", nil
	}

	return "", err
}

func checkUtilInContainer(clientset *kubernetes.Clientset, config *rest.Config, namespace, podName, containerName string, util string) (bool, error) {
	var stdout, stderr bytes.Buffer
	retCode, err := exec(clientset, config, namespace, podName, containerName, util, nil, &stdout, &stderr, false)
	return retCode != 127 && retCode != 128, err
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

func getPod(podName, namespace string) (*corev1.Pod, error) {
	_pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metaV1.GetOptions{})
	return _pod, err
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
	log(fmt.Sprintf("[+] Found %d pods in %d deployments\n", len(deploymentPods), len(deployments.Items)))

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
	log(fmt.Sprintf("[+] Found %d pods in %d statefulsets\n", len(statefulSetsPods), len(statefulSets.Items)))

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
				container.shell, _ = getShellInContainer(clientset, config, namespace, container.podName, container.containerName)
				container.testable = checkUtilsv2(clientset, config, container.podName, container.containerName, namespace, utils) && container.shell != ""
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
		log(fmt.Sprintf(prompt))
		_, err := fmt.Scanf("%s\n", &response)

		if err != nil {
			log(fmt.Sprintln("Error reading input. Please try again."))
			continue
		}

		response = strings.ToUpper(strings.TrimSpace(response))

		if response == "Y" {
			return true
		} else if response == "N" {
			return false
		} else {
			log(fmt.Sprintln("Invalid input. Please enter 'Y' or 'N'."))
		}
	}
}

func Init() error {
	var err error

	if _, ok := map[string]int{"ansi": 0, "text": 0, "html": 0}[format]; !ok {
		//fmt.Fprintln(os.Stderr, "Invalid value of the output format option '-o'. Valid values are ansi, text or html\n")
		//flag.Usage()
		return errors.New("Invalid value of the output format option '-o'. Valid values are ansi, text or html")
	}

	config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		//fmt.Println(err.Error())
		return err
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		//fmt.Println(err.Error())
		//os.Exit(1)
		return err
	}

	logWg.Add(1)
	go func() {
		defer logWg.Done()
		for msg := range logBuffer {
			fmt.Fprint(os.Stderr, msg)
		}
	}()

	return nil
}

func saveScan(podName, containerName string, scanReport bytes.Buffer) error {
	fileName := fmt.Sprintf("%s-%s-%s.%s", podName, containerName, time.Now().Format("2006-01-02-150405"), format)
	fileName = filepath.Join(directory, fileName)

	var report []byte
	switch format {
	case "html":
		report = []byte(htmlHeader)
		report = append(report, ansihtml.ConvertToHTML(scanReport.Bytes())...)
		report = append(report, []byte(htmlFooter)...)
	default:
		report = scanReport.Bytes()
	}

	err := os.WriteFile(fileName, report, 0666)
	if err != nil {
		//log(fmt.Sprintf("[!!] Cannot save scan report for %s/%s container to file %s\n", podName, containerName, fileName))
		//fmt.Println(scanReport.String())
		return err
	}
	return nil
}

func scan(pods []corev1.Pod, namespace string) error {
	log(fmt.Sprintln("[*] Identifying testable containers"))
	targetContainers, nontestableContainers = verifyContainers(pods)
	log(fmt.Sprintf("[+] Found %d unique containers\n", len(targetContainers)+len(nontestableContainers)))

	if len(targetContainers) > 0 {
		log(fmt.Sprintf("[+] Following %d containers can be tested:\n", len(targetContainers)))
		var buf bytes.Buffer
		w := tabwriter.NewWriter(&buf, 0, 0, 1, ' ', 0)
		for _, list := range targetContainers {
			fmt.Fprintf(w, "%s\t%s\n", list.podName, list.containerName)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
		log(buf.String())
	} else {
		return errors.New("[-] Did not find any testable containers")
		//log(fmt.Sprintln("[-] Did not find any testable containers"))
		//return
	}

	if len(nontestableContainers) > 0 {
		log(fmt.Sprintf("[-] Following %d containers cannot be tested:\n", len(nontestableContainers)))
		var buf bytes.Buffer
		w := tabwriter.NewWriter(&buf, 0, 0, 1, ' ', 0)
		for _, container := range nontestableContainers {
			fmt.Fprintf(w, "%s\t%s\n", container.podName, container.containerName)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
		log(buf.String())
	}

	if !quiet {
		if promptYN("\nDo you wish to proceed with testing? (Y/N): ") {
			log(fmt.Sprintln("Proceeding with testing..."))
		} else {
			//log(fmt.Sprintln("Action cancelled."))
			return errors.New("Action cancelled.")
		}
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
					if format == "text" {
						shell = fmt.Sprintf("%s -s -- -c", shell)
					}
					_, err := exec(clientset, config, namespace, container.podName, container.containerName, shell, lsescript, &stdout, &stderr, false)
					if err != nil {
						log(err.Error())
					}
					resultsProdChan <- Result{container.podName, container.containerName, stdout}
				}
			}()
		}

		resultsCollectorWg.Add(1)
		go func() {
			var cnt int

			defer resultsCollectorWg.Done()
			for result := range resultsProdChan {
				if err := saveScan(result.podName, result.containerName, result.scanReport); err != nil {
					log(err.Error())
					log(result.scanReport.String())
				}
				cnt++
				log(fmt.Sprintf("\rAnalyzed %d containers", cnt))
			}
			log(fmt.Sprintf("\n"))
		}()

		contFanOutWg.Wait()
		close(contProdChan)
		testWorkerWg.Wait()
		close(resultsProdChan)
		resultsCollectorWg.Wait()
	}
	return nil
}

func scanPod(podName, namespace string) error {
	log(fmt.Sprintln("[+] Started"))
	log(fmt.Sprintf("[+] Searching for %s pod\n", podName))
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metaV1.GetOptions{})
	if err != nil {
		return err
	}
	log(fmt.Sprintf("[+] Pod %s found\n", podName))
	err = scan([]corev1.Pod{*pod}, namespace)
	return err
}

func scanContainer(podName, containerName, namespace string) error {
	log(fmt.Sprintln("[+] Started"))
	log(fmt.Sprintf("[+] Searching for %s/%s container\n", podName, containerName))
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metaV1.GetOptions{})
	if err != nil {
		return err
	}
	found := false
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			found = true
			break
		}
	}
	if !found {
		//log(fmt.Sprintf("[+] Container %s/%s not found. Aborting\n", podName, containerName))
		return errors.New(fmt.Sprintf("[+] Container %s/%s not found. Aborting\n", podName, containerName))
	}

	log(fmt.Sprintf("[+] Container %s/%s found\n", podName, containerName))
	// this is necessary, when cross-compiling on windows
	lsetmp := bytes.Replace(lse, []byte("\r\n"), []byte("\n"), -1)
	lsetmp = bytes.Replace(lsetmp, []byte("\r"), []byte(""), -1)

	lsescript := bytes.NewBuffer(lsetmp)

	shell, _ := getShellInContainer(clientset, config, namespace, podName, containerName)
	testable := checkUtilsv2(clientset, config, podName, containerName, namespace, utils) && shell != ""

	if !testable {
		//log(fmt.Sprintf("[!!] Container %s/%s is not testable. Aborting.\n", podName, containerName))
		//os.Exit(1)
		return errors.New(fmt.Sprintf("[!!] Container %s/%s is not testable. Aborting.\n", podName, containerName))
	}

	if format == "text" {
		shell = fmt.Sprintf("%s -s -- -c", shell)
	}

	log(fmt.Sprintf("[+] Container %s/%s is testable.\n[+] Proceeding with tesitng.\n", podName, containerName))
	var stdout, stderr bytes.Buffer

	_, err = exec(clientset, config, namespace, podName, containerName, shell, lsescript, &stdout, &stderr, false)
	if err != nil {
		//log(fmt.Sprintln(err))
		//os.Exit(1)
		return err
	}
	err = saveScan(podName, containerName, stdout)
	return err
}

func scanNamespace(namespace string) error {
	log(fmt.Sprintln("[+] Started"))
	log(fmt.Sprintln("[+] Creating a list of unique pods"))

	//pods, err := getPods(clientset, *namespace, metaV1.ListOptions{})
	podCount, pods, err := getUniquePods(clientset, namespace)
	if err != nil {
		//fmt.Fprintln(os.Stderr, err)
		//os.Exit(1)
		return err
	}

	if len(pods) == 0 {
		//log(fmt.Sprintf("[-] No pods found in namespace %q\n", namespace))
		//os.Exit(1)
		return errors.New(fmt.Sprintf("[-] No pods found in namespace %q\n", namespace))
	}
	log(fmt.Sprintf("[+] Found %d unique pods out of %d pods in %s namespace\n", len(pods), podCount, namespace))
	err = scan(pods, namespace)
	return err
}

func listContainers() error {
	log(fmt.Sprintln("[+] Started"))
	log(fmt.Sprintf("[+] Creating a list of pods/containers for %s namespace\n", namespace))

	switch {
	case pod != "":
		_pod, err := getPod(pod, namespace)
		if err != nil {
			//log(err.Error())
			return err
		}
		var buf bytes.Buffer

		log(fmt.Sprintf("[+] Found %d containers in %s pod\n", len(_pod.Spec.Containers), _pod.Name))
		t := table.NewWriter()
		t.SetOutputMirror(&buf)
		t.AppendHeader(table.Row{"#", "Pod", "Container"})
		for idx, cntr := range _pod.Spec.Containers {
			t.AppendRows([]table.Row{{idx + 1, _pod.Name, cntr.Name}})
			t.AppendSeparator()
		}
		t.Render()
		log(buf.String())
	case pod == "":
		pods, err := getPods(clientset, namespace, metaV1.ListOptions{})
		if err != nil {
			return err
		}
		var buf bytes.Buffer

		t := table.NewWriter()
		t.SetOutputMirror(&buf)
		t.AppendHeader(table.Row{"#", "Pod", "Container"})
		cnt := 1
		for _, _pod := range pods {
			for _, cntr := range _pod.Spec.Containers {
				t.AppendRows([]table.Row{{cnt, _pod.Name, cntr.Name}})
				cnt++
			}
			t.AppendSeparator()
		}

		t.Render()
		log(buf.String())
	}
	return nil
}

func run() error {
	// go executes defer statements in thenLIFO order
	defer logWg.Wait()
	defer close(logBuffer)

	if err := Init(); err != nil {
		//log(err.Error())
		return err
	}

	switch {
	case list:
		return listContainers()
	case pod != "" && container == "":
		if err := scanPod(pod, namespace); err != nil {
			//log(err.Error())
			return err
		}
	case pod != "" && container != "":
		if err := scanContainer(pod, container, namespace); err != nil {
			//log(err.Error())
			return err
		}
	case pod == "" && container == "":
		if err := scanNamespace(namespace); err != nil {
			//log(err.Error())
			return err
		}
	}
	return nil
}

func main() {

	var cmd = &cobra.Command{
		Use:   "k8slse",
		Short: "k8slse is a command line application that enumerates containers with Linux Smart Enumeration script",
		Long: `
This application enumerates containers in k8s environment with the Linux Smart Enumeration script. 
It allows to enumerate all containers from a given namespace, or a selected pod. It can also enumerate a specific container.
It allows to enumerate all containers from a given namespace, or a selected pod. It can also enumerate a specific container.
It saves an enumeration report for each container separately in a file. The report can be saved in 
a plain text, ansi or html output format.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run()
		},
	}

	if home := homedir.HomeDir(); home != "" {
		cmd.Flags().StringVarP(&kubeconfig, "kubeconfig", "k", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		cmd.Flags().StringVarP(&kubeconfig, "kubeconfig", "k", "", "absolute path to the kubeconfig file")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
	cmd.Flags().StringVarP(&directory, "directory", "d", workingDirectory, "a directory where reports should be saved to")
	cmd.Flags().StringVarP(&format, "output", "o", "ansi", "Output format: ansi, text, or html")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "CNF namespace")
	cmd.Flags().StringVarP(&pod, "pod", "p", "", "a pod name, if not provided then all containers in a namespace will be enumerated.")
	cmd.Flags().StringVarP(&container, "container", "c", "", "a container name")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "quiet execution - no status information")
	cmd.Flags().BoolVarP(&list, "list", "l", false, "list containers")

	// Disable automatic printing of usage when an error occurs
	cmd.SilenceUsage = true

	// Custom PreRunE to check for parse errors
	cmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// This checks for any parse errors
		if err := cmd.ParseFlags(args); err != nil {
			return err
		}
		return nil
	}

	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		// When a non-existing option is invoked, print the usage
		if err := c.Usage(); err != nil {
			fmt.Fprintf(os.Stderr, "Error printing usage: %v\n", err)
		}
		// Return the original error to stop execution
		return err
	})

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
