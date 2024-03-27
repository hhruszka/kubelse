package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
	"k8s.io/client-go/util/homedir"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/tabwriter"
)

type container struct {
	podName       string
	containerName string
	shell         string
}

var (
	debug                                   bool
	kubeconfig                              *string
	namespace                               *string
	config                                  *rest.Config
	clientset                               *kubernetes.Clientset
	utils                                   []string
	targetContainers, nontestableContainers []container
	logBuffer                               bytes.Buffer
)

//go:embed data/lse.sh
var lse []byte

func exec(clientset *kubernetes.Clientset, config *rest.Config, namespace string, podName string, containerName string, cmd string, stdin *bytes.Buffer, stdout *bytes.Buffer, stderr *bytes.Buffer, tty bool) error {
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
	//Param("container", containerName).
	//Param("stdout", "true").
	//Param("stderr", "true").
	//Param("command", cmd).
	//Param("command", "--version")

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[-] Execution failed with error code %d\n", err)
		}
		return err
	}

	//var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    tty,
	})
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

func checkUtilInContainer(clientset *kubernetes.Clientset, config *rest.Config, namespace, podName, containerName string, util string) bool {
	commandArgs := strings.Fields(util)
	req := clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   commandArgs,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[-][%s:%s] Execution of %q failed with error  %s\n", podName, containerName, util, err)
		}
		return false
	}

	// Redirecting standard streams to bytes.Buffer prevents them from printing in console.
	// They could be directed to e.g. os.Stdin etc.
	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})

	if err != nil {
		if debug {
			var e k8sexec.CodeExitError
			switch {
			case errors.As(err, &e):
				fmt.Fprintf(os.Stderr, "[-][%s:%s] Execution of %q failed with error code: %d -> %s\n", podName, containerName, util, e.ExitStatus(), e.String())
				if stdout.Len() > 0 {
					fmt.Fprintf(os.Stderr, "[-][%s:%s] Following output was returned:\n%s\n", podName, containerName, stdout.String())
				}
			default:
				fmt.Fprintf(os.Stderr, "[-][%s:%s] Execution of %q failed with error %s\n", podName, containerName, util, err)
				if stdout.Len() > 0 {
					fmt.Fprintf(os.Stderr, "[-][%s:%s] Following output was returned:\n%s\n", podName, containerName, stdout.String())
				}
				xType := reflect.TypeOf(err)
				fmt.Fprintln(os.Stderr, "[-][%s:%s] 1: ", xType.String(), xType.PkgPath(), xType.Kind(), xType.Kind() == reflect.Ptr)
			}
		}
		return false
	}

	// If stdout is not empty, the shell exists.
	//found = stdout.Len() > 0 || err == nil
	//return found
	// no errors where returned from the execution thus the command exists
	// using stdout.Len() to determine if command exists is wrong since it also contains error messages.
	return err == nil
}

type UtilsStatus map[string]map[string]map[string]bool

var status UtilsStatus

func checkUtil(clientset *kubernetes.Clientset, config *rest.Config, podName string, containerName string, namespace string, utils []string) {
	for _, util := range utils {
		utilFound := checkUtilInContainer(clientset, config, namespace, podName, containerName, util)
		status[podName][containerName][util] = utilFound
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

func getPods() (*corev1.PodList, error) {
	var pods *corev1.PodList
	pods, err := clientset.CoreV1().Pods(*namespace).List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return pods, nil
}

func verifyContainers(pods *corev1.PodList) (target []container, nontestable []container) {
	status = make(UtilsStatus)

	if len(utils) == 0 {
		return nil, nil
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == "Running" {
			status[pod.Name] = make(map[string]map[string]bool)
			for _, container := range pod.Spec.Containers {
				status[pod.Name][container.Name] = make(map[string]bool)
				checkUtil(clientset, config, pod.Name, container.Name, *namespace, utils)
			}
		}
	}

	if len(status) > 0 {
		for pod, containers := range status {
			for container, utilsStatus := range containers {
				canBeTested := true
				for _, present := range utilsStatus {
					canBeTested = canBeTested && present
				}
				if canBeTested {
					testableContainers = append(testableContainers, containerList{podName: pod, containerName: container})
				} else {
					nontestableContainers = append(nontestableContainers, containerList{podName: pod, containerName: container})
				}
			}
		}
	}
}

func main() {
	pods, err := getPods()
	if err != nil {
		fmt.Println(err)
		os.Exit(0)
	}
	if len(pods.Items) == 0 {
		fmt.Printf("[-] No pods found in namespace %q\n", *namespace)
		os.Exit(0)
	}

	if len(testableContainers) > 0 {
		fmt.Println("Following containers can be tested:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
		for _, list := range testableContainers {
			fmt.Fprintf(w, "%s\t%s\n", list.podName, list.containerName)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
	}

	if len(nontestableContainers) > 0 {
		fmt.Println("Following containers cannot be tested:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
		for _, list := range nontestableContainers {
			fmt.Fprintf(w, "%s\t%s\n", list.podName, list.containerName)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
	}
}
