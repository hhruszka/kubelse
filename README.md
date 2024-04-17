
This application enumerates containers in k8s environment with the [Linux Smart Enumeration script](https://github.com/diego-treitos/linux-smart-enumeration?tab=readme-ov-file).
It allows to enumerate all containers from a given namespace, selected pods or containers of a single pod. It saves an enumeration report for each container separately in a file. The report can be saved in a plain text, ansi or html output format.

### Usage
```
kubelse [options]

Options:
  -c, --containers string   a container or comma-separated containers to be enumerated
  -d, --directory string    a directory where reports should be saved to (default "/Users/hhruszka/GolandProjects/kubelse")
  -h, --help                help for kubelse-macos-arm64
  -k, --kubeconfig string   (optional) absolute path to the kubeconfig file (default "/Users/hhruszka/.kube/config")
  -l, --list                list containers, no enumeration
  -n, --namespace string    a namespace (default "default")
  -o, --output string       Output format: ansi, text, or html (default "ansi")
  -p, --pods string         a pod or comma-separated pods, which containers are to be enumerated, if not provided then all containers in a namespace will be enumerated.
  -q, --quiet               quiet execution - no status information
  -v, --version             prints kubelse-macos-arm64 version

```

### Examples

Test all unique pods' containers in a 'my-namespace' namespace
```
./kubelse -n my-namespace
```

Test containers of a pod "pod1" in a 'my-namespace' namespace
```
./kubelse -n my-namespace -p pod1
```

Test all unique pods' containers in a 'my-namespace' namespace, save reports in 'html' formate in a directory "/tmp/report"
```
./kubelse -n my-namespace -o html -d /tmp/report
```
