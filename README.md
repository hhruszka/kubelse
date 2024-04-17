
This application enumerates containers in k8s environment with the [Linux Smart Enumeration script](https://github.com/diego-treitos/linux-smart-enumeration?tab=readme-ov-file).
It allows to enumerate all containers from a given namespace, or a selected pod. It can also enumerate a specific container.
It allows to enumerate all containers from a given namespace, or a selected pod. It can also enumerate a specific container.
It saves an enumeration report for each container separately in a file. The report can be saved in
a plain text, ansi or html output format.

### Usage
```
bptcnflse [flags]

Options:
  -c, --container string    a container name
  -d, --directory string    a directory where reports should be saved to (default "/home/user" )
  -h, --help                help for bptcnflse-windows-amd64.exe
  -k, --kubeconfig string   (optional) absolute path to the kubeconfig file (default "~/.kube/config")
  -l, --list                list containers
  -n, --namespace string    CNF namespace (default "default")
  -o, --output string       Output format: ansi, text, or html (default "ansi")
  -p, --pod string          a pod name, if not provided then all containers in a namespace will be enumerated.
  -q, --quiet               quiet execution - no status information
  -v, --version             prints bptcnflse version
```

### Examples

Test all unique pods' containers in a 'my-namespace' namespace
```
./bptcnflse -n my-namespace
```

Test containers of a pod "pod1" in a 'my-namespace' namespace
```
./bptcnflse -n my-namespace -p pod1
```

Test all unique pods' containers in a 'my-namespace' namespace, save reports in 'html' formate in a directory "/tmp/report"
```
./bptcnflse -n my-namespace -o html -d /tmp/report
```
