package cmd

import (
	"errors"
	"fmt"
	"github.com/hhruszka/k8sexec"
	"github.com/spf13/cobra"
	"k8s.io/client-go/util/homedir"
	"os"
	"path/filepath"
)

// CLI options variables
var (
	debug         bool
	kubeconfig    string
	namespace     string
	format        string
	podscli       string
	containerscli string
	directory     string
	quiet         bool
	version       bool
	list          bool
)

var appName string = filepath.Base(os.Args[0])
var AppVersion string

func run() error {
	// go executes defer statements in the LIFO order
	defer stoplog()

	if version {
		fmt.Println(appName, AppVersion)
		return nil
	}

	k8sExecClient, err := k8sexec.NewK8SExec(kubeconfig, namespace)
	if err != nil {
		return fmt.Errorf("Internal application error: %s\n", err.Error())
	}

	if list {
		return listContainers(k8sExecClient)
	}

	containers, err := getContainers(k8sExecClient, untangleOption(podscli), untangleOption(containerscli))
	if err != nil {
		return err
	}
	return scanContainers(k8sExecClient, containers)
}

var cmd = &cobra.Command{
	Use:   appName + " [flags]",
	Short: appName + " is a command line application that enumerates containers with Linux Smart Enumeration script",
	Long: `
This application enumerates containers in k8s environment with the Linux Smart Enumeration script. 
It allows to enumerate all containers from a given namespace, selected pods or selected containers of a given pod.
It saves an enumeration report for each container separately in a file. The report can be saved in 
a plain text, ansi or html output format.`,
	SilenceErrors: true,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		// verify value of 'format' option
		if format != "ansi" && format != "text" && format != "json" {
			return errors.New("Invalid value of the output format option '-o'. Valid values are ansi, text or html")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return run()
	},
}

func init() {
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
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "a namespace")
	cmd.Flags().StringVarP(&podscli, "pods", "p", "", "a pod or comma-separated pods, which containers are to be enumerated, if not provided then all containers in a namespace will be enumerated.")
	cmd.Flags().StringVarP(&containerscli, "containers", "c", "", "a container or comma-separated containers to be enumerated")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "quiet execution - no status information")
	cmd.Flags().BoolVarP(&version, "version", "v", false, "prints "+appName+" version")
	cmd.Flags().BoolVarP(&list, "list", "l", false, "list containers, no enumeration executed")

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
}

func Execute() error {
	return cmd.Execute()
}
