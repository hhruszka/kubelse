package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/hhruszka/k8sexec"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/robert-nix/ansihtml"
	corev1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8slse/data"
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

type Container struct {
	Pod       string `json:"Pod"`
	Container string `json:"Container"`
}

type ContainerInfo struct {
	container Container
	shell     string
	testable  bool
}

type Result struct {
	podName       string
	containerName string
	scanReport    []string
}

// utils                                   []string = []string{"stat /usr/bin/find", "stat /bin/cat", "stat /bin/ps", "stat /bin/grep"}
// App global variables
var (
	config                *rest.Config
	clientset             *kubernetes.Clientset
	utils                 []string = []string{"stat /usr/bin/find", "stat /bin/cat", "stat /bin/grep"}
	targetContainers      []ContainerInfo
	nontestableContainers []ContainerInfo
)

// lse script is embeded in data package
var lse []byte = data.GetScript()

// checkShellsInContainer checks for the presence of specified shells in the given container of a pod.
func getShellInContainer(k8s *k8sexec.K8SExec, container Container) (string, error) {
	execStatus := k8s.Exec(container.Pod, container.Container, strings.Fields("sh --version"), nil)

	if execStatus.RetCode == k8sexec.Success {
		return "sh", nil
	}

	execStatus = k8s.Exec(container.Pod, container.Container, strings.Fields("bash --version"), nil)
	if execStatus.RetCode == k8sexec.Success {
		return "bash", nil
	}

	return "", fmt.Errorf(strings.Join(execStatus.Error, "\n"))
}

func checkUtilInContainer(k8s *k8sexec.K8SExec, container Container, util string) (bool, error) {
	execStatus := k8s.Exec(container.Pod, container.Container, strings.Fields(util), nil)
	return execStatus.RetCode != k8sexec.CommandNotFound && execStatus.RetCode != k8sexec.CommandCannotExecute, fmt.Errorf(strings.Join(execStatus.Error, "\n"))
}

func checkUtils(k8s *k8sexec.K8SExec, container Container, utils []string) bool {
	var utilFound bool = true
	for _, util := range utils {
		result, _ := checkUtilInContainer(k8s, container, util)
		utilFound = utilFound && result
		if result == false {
			break
		}
	}
	return utilFound
}

func verifyContainers(k8s *k8sexec.K8SExec, containers []Container) (target []ContainerInfo, nontestable []ContainerInfo) {
	var (
		podProdChan chan ContainerInfo = make(chan ContainerInfo, len(containers))
		conProdChan chan ContainerInfo = make(chan ContainerInfo, runtime.NumCPU())
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
	for i := 0; i < len(containers); i++ {
		contVerWorkerWg.Add(1)
		go func() {
			defer contVerWorkerWg.Done()
			for container := range podProdChan {
				container.shell, _ = getShellInContainer(k8s, container.container)
				container.testable = checkUtils(k8s, container.container, utils) && container.shell != ""
				conProdChan <- container
			}
		}()
	}

	// this goroutine distributes found pods through podProdChan for workers that check shell and utilities
	podWg.Add(1)
	go func() {
		defer podWg.Done()
		for _, container := range containers {
			podProdChan <- ContainerInfo{container: container}
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

func saveScan(podName, containerName string, scanReport []string) error {
	fileName := fmt.Sprintf("%s-%s-%s.%s", podName, containerName, time.Now().Format("2006-01-02-150405"), format)
	fileName = filepath.Join(directory, fileName)

	var report []byte
	switch format {
	case "html":
		report = []byte(htmlHeader)
		report = append(report, ansihtml.ConvertToHTML([]byte(strings.Join(scanReport, "\n")))...)
		report = append(report, []byte(htmlFooter)...)
	default:
		report = []byte(strings.Join(scanReport, "\n"))
	}

	err := os.WriteFile(fileName, report, 0666)
	if err != nil {
		return err
	}
	return nil
}

func scan(k8s *k8sexec.K8SExec, containers []Container) error {
	log(fmt.Sprintln("[*] Identifying containers that can be tested"))
	targetContainers, nontestableContainers = verifyContainers(k8s, containers)
	log(fmt.Sprintf("[+] Found %d containers\n", len(targetContainers)+len(nontestableContainers)))

	if len(targetContainers) > 0 {
		log(fmt.Sprintf("[+] Following %d containers can be tested:\n", len(targetContainers)))
		var buf bytes.Buffer
		w := tabwriter.NewWriter(&buf, 0, 0, 1, ' ', 0)
		for _, list := range targetContainers {
			fmt.Fprintf(w, "%s\t%s\n", list.container.Pod, list.container.Container)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
		log(buf.String())
	} else {
		return errors.New("[-] Did not find any containers that can be tested")
	}

	if len(nontestableContainers) > 0 {
		log(fmt.Sprintf("[-] Following %d containers cannot be tested:\n", len(nontestableContainers)))
		var buf bytes.Buffer
		w := tabwriter.NewWriter(&buf, 0, 0, 1, ' ', 0)
		for _, container := range nontestableContainers {
			fmt.Fprintf(w, "%s\t%s\n", container.container.Pod, container.container.Container)
		}
		fmt.Fprintln(w, "\t")
		w.Flush()
		log(buf.String())
	}

	if !quiet {
		if promptYN("\nDo you wish to proceed with testing? (Y/N): ") {
			log(fmt.Sprintln("Proceeding with testing..."))
		} else {
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
			testWorkerWg.Add(1)
			go func() {
				defer testWorkerWg.Done()
				for container := range contProdChan {
					lsescript := bytes.NewBuffer(lsetmp)
					shell := container.shell
					if format == "text" {
						shell = fmt.Sprintf("%s -s -- -c", shell)
					}
					execStatus := k8s.Exec(container.container.Pod, container.container.Container, strings.Fields(shell), lsescript)
					if execStatus.RetCode != k8sexec.Success {
						log(strings.Join(execStatus.Error, "\n"))
					}
					resultsProdChan <- Result{container.container.Pod, container.container.Container, execStatus.Stdout}
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
					log(strings.Join(result.scanReport, "\n"))
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

func scanContainers(k8s *k8sexec.K8SExec, containers []Container) error {
	log(fmt.Sprintln("[+] Started"))
	log(fmt.Sprintln("[+] Creating a list of unique pods"))

	if len(containers) == 0 {
		return errors.New(fmt.Sprintf("[-] No pods/containers found in namespace %q\n", namespace))
	}
	log(fmt.Sprintf("[+] Found %d containers in %s namespace\n", len(containers), namespace))
	return scan(k8s, containers)
}

func listContainers(k8s *k8sexec.K8SExec) error {
	var pods []corev1.Pod
	log(fmt.Sprintln("[+] Started"))
	log(fmt.Sprintf("[+] Creating a list of pods/containers for %s namespace\n", namespace))

	if podscli != "" {
		for _, pod := range untangleOption(podscli) {
			_pod, err := k8s.GetPod(pod, metaV1.GetOptions{})
			if err != nil {
				return err
			}
			pods = append(pods, *_pod)
		}
	} else {
		var err error

		_, pods, err = k8s.GetUniquePods()
		if err != nil {
			return err
		}
	}

	var buf bytes.Buffer

	t := table.NewWriter()
	t.SetOutputMirror(&buf)
	t.AppendHeader(table.Row{"#", "Pod", "Container"})

	for _, pod := range pods {
		t.AppendRow(table.Row{pod.Name, "", ""}, table.RowConfig{AutoMerge: true, AutoMergeAlign: text.AlignLeft})
		t.AppendSeparator()
		for idx, container := range pod.Spec.Containers {
			t.AppendRows([]table.Row{{idx + 1, pod.Name, container.Name}})
			t.AppendSeparator()
		}
	}
	t.Render()
	log(buf.String())

	return nil
}

func getContainers(k8s *k8sexec.K8SExec, pods []string, containers []string) ([]Container, error) {
	var containerList []Container

	if len(pods) > 1 && len(containers) > 0 {
		return nil, fmt.Errorf("List of containers to be tested can be provided only for a single pod\n")
	}

	if len(pods) == 1 && len(containers) > 0 {
		for _, container := range containers {
			containerList = append(containerList, Container{pods[0], container})
		}
	}

	if len(pods) >= 1 && len(containers) == 0 {
		for _, pod := range pods {
			foundPod, err := k8s.GetPod(pod, metaV1.GetOptions{})
			if err != nil {
				return nil, err
			}
			if foundPod.Status.Phase != "Running" {
				continue
			}
			for _, container := range foundPod.Spec.Containers {
				containerList = append(containerList, Container{foundPod.Name, container.Name})
			}
		}
	}

	if len(pods) == 0 && len(containers) == 0 {
		_, pods, err := k8s.GetUniquePods()
		if err != nil {
			return nil, err
		}
		for _, pod := range pods {
			if pod.Status.Phase != "Running" {
				continue
			}
			for _, container := range pod.Spec.Containers {
				containerList = append(containerList, Container{pod.Name, container.Name})
			}
		}

	}
	return containerList, nil
}
