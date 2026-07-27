// cluster-join: automate k3s agent node join and verify node Ready status.
//
// This tool automates the steps from setup/03-k3s-expand.md:
//   1. Verify the control plane is reachable
//   2. Generate the k3s agent install command
//   3. Wait for the new node to appear as Ready
//   4. Label the node with hardware information
//
// Build: go build -o cluster-join .
// Run:   sudo ./cluster-join --server https://192.168.1.101:6443 \
//                            --token K10...::server:... \
//                            --node-name rpi5-02 \
//                            --node-ip 192.168.1.102

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var (
	flagServer   = flag.String("server", "", "k3s server URL (e.g. https://192.168.1.101:6443)")
	flagToken    = flag.String("token", "", "k3s server token (from /var/lib/rancher/k3s/server/node-token)")
	flagNodeName = flag.String("node-name", "", "Name for the new node (e.g. rpi5-02)")
	flagNodeIP   = flag.String("node-ip", "", "IP address for the new node (e.g. 192.168.1.102)")
	flagDryRun   = flag.Bool("dry-run", false, "Print the install command without executing it")
	flagWait     = flag.Duration("wait", 3*time.Minute, "Time to wait for node to become Ready")
	flagKubeConf = flag.String("kubeconfig", "/etc/rancher/k3s/k3s.yaml", "Path to kubeconfig on control plane")
)

// validHostname matches RFC 1123 hostnames.
var validHostname = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?$`)

// validToken matches k3s tokens (alphanumeric plus : and :: separators).
var validToken = regexp.MustCompile(`^[A-Za-z0-9:_\-]+$`)

// NodeStatus represents a Kubernetes node's Ready condition.
type NodeStatus struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		NodeInfo struct {
			Architecture            string `json:"architecture"`
			KernelVersion           string `json:"kernelVersion"`
			ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
			KubeletVersion          string `json:"kubeletVersion"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

func main() {
	flag.Parse()

	if *flagServer == "" || *flagToken == "" {
		fmt.Fprintln(os.Stderr, "usage: cluster-join --server <url> --token <token> [--node-name <name>] [--node-ip <ip>]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Get the token from the control plane node:")
		fmt.Fprintln(os.Stderr, "  sudo cat /var/lib/rancher/k3s/server/node-token")
		os.Exit(1)
	}

	// Validate all inputs before use — prevent injection via malformed values.
	if err := validateInputs(*flagServer, *flagToken, *flagNodeName, *flagNodeIP); err != nil {
		fmt.Fprintf(os.Stderr, "invalid input: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("=== k3s Cluster Node Join ===")
	fmt.Println()

	// Step 1: Verify control plane connectivity
	fmt.Printf("Step 1: Checking control plane connectivity at %s...\n", *flagServer)
	if err := checkConnectivity(*flagServer); err != nil {
		host := serverHost(*flagServer)
		fmt.Fprintf(os.Stderr, "  ✗ Cannot reach control plane: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Check: ping %s\n", host)
		os.Exit(1)
	}
	fmt.Printf("  ✓ Control plane reachable\n\n")

	// Step 2: Show the equivalent manual command (for transparency)
	fmt.Println("Step 2: Equivalent manual install command (for reference):")
	printInstallCmd(*flagServer, *flagToken, *flagNodeName, *flagNodeIP)
	fmt.Println()

	if *flagDryRun {
		fmt.Println("(dry-run: not executing install command)")
		return
	}

	// Step 3: Download and run k3s installer using environment variables.
	// We do NOT use sh -c with a concatenated string. Instead:
	//   - Download the install script with curl into a temp file
	//   - Execute sh with the script file path as an argument
	//   - Pass configuration as environment variables (no shell interpolation)
	fmt.Println("Step 3: Installing k3s agent...")

	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "  ✗ Must run as root (sudo ./cluster-join ...)")
		os.Exit(1)
	}

	if err := downloadAndInstall(*flagServer, *flagToken, *flagNodeName, *flagNodeIP); err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ Install failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  ✓ k3s agent installed\n\n")

	// Step 4: Wait for node Ready
	nodeName := *flagNodeName
	if nodeName == "" {
		hostname, _ := os.Hostname()
		nodeName = hostname
	}

	fmt.Printf("Step 4: Waiting for node %q to become Ready (timeout: %s)...\n",
		nodeName, *flagWait)

	ctx, cancel := context.WithTimeout(context.Background(), *flagWait)
	defer cancel()

	node, err := waitForNodeReady(ctx, nodeName, *flagKubeConf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ Node did not become Ready: %v\n", err)
		fmt.Fprintln(os.Stderr, "  Debug: kubectl get nodes")
		fmt.Fprintln(os.Stderr, "  Debug: kubectl describe node "+nodeName)
		os.Exit(1)
	}

	fmt.Printf("  ✓ Node %q is Ready\n\n", nodeName)

	// Step 5: Print node info
	fmt.Println("Step 5: Node information:")
	fmt.Printf("  Architecture     : %s\n", node.Status.NodeInfo.Architecture)
	fmt.Printf("  Kernel           : %s\n", node.Status.NodeInfo.KernelVersion)
	fmt.Printf("  Container runtime: %s\n", node.Status.NodeInfo.ContainerRuntimeVersion)
	fmt.Printf("  Kubelet version  : %s\n", node.Status.NodeInfo.KubeletVersion)
	fmt.Println()

	// Step 6: Label with hardware info
	labels := buildNodeLabels(node)
	if len(labels) > 0 {
		fmt.Println("Step 6: Applying hardware labels:")
		for k, v := range labels {
			fmt.Printf("  %s=%s\n", k, v)
		}
		if err := applyLabels(nodeName, labels, *flagKubeConf); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠  Label apply failed: %v\n", err)
		} else {
			fmt.Printf("  ✓ Labels applied\n")
		}
	}

	fmt.Println()
	fmt.Println("=== Join Complete ===")
	fmt.Printf("Run: kubectl get nodes -o wide\n")
}

// validateInputs checks all user-provided values before use.
func validateInputs(server, token, nodeName, nodeIP string) error {
	// Server must be https://host:port
	if !strings.HasPrefix(server, "https://") {
		return fmt.Errorf("server URL must start with https://")
	}
	host := serverHost(server)
	if net.ParseIP(host) == nil && !validHostname.MatchString(host) {
		return fmt.Errorf("server host %q is not a valid IP or hostname", host)
	}

	// Token: k3s tokens are hex strings with :: separators
	if !validToken.MatchString(token) {
		return fmt.Errorf("token contains invalid characters")
	}

	// Node name: must be a valid hostname
	if nodeName != "" && !validHostname.MatchString(nodeName) {
		return fmt.Errorf("node-name %q is not a valid hostname", nodeName)
	}

	// Node IP: must parse as valid IP
	if nodeIP != "" && net.ParseIP(nodeIP) == nil {
		return fmt.Errorf("node-ip %q is not a valid IP address", nodeIP)
	}

	return nil
}

// printInstallCmd shows the equivalent shell command for transparency.
// The actual install uses downloadAndInstall which avoids shell injection.
func printInstallCmd(server, token, nodeName, nodeIP string) {
	fmt.Printf("  # Environment variables passed to k3s installer:\n")
	fmt.Printf("  K3S_URL=%s \\\n", server)
	fmt.Printf("  K3S_TOKEN=<token> \\\n")
	if nodeName != "" {
		fmt.Printf("  K3S_NODE_NAME=%s \\\n", nodeName)
	}
	if nodeIP != "" {
		fmt.Printf("  INSTALL_K3S_EXEC=\"agent --node-ip=%s\" \\\n", nodeIP)
	}
	fmt.Printf("  sh /tmp/k3s-install.sh\n")
}

// downloadAndInstall fetches the k3s install script and runs it with
// configuration passed as environment variables — not via shell interpolation.
func downloadAndInstall(server, token, nodeName, nodeIP string) error {
	// Download install script
	scriptPath, err := downloadInstallScript()
	if err != nil {
		return fmt.Errorf("download install script: %w", err)
	}
	defer os.Remove(scriptPath)

	// Build INSTALL_K3S_EXEC value (agent subcommand + flags)
	execArgs := "agent"
	if nodeIP != "" {
		// net.ParseIP already validated nodeIP — safe to use directly
		execArgs += " --node-ip=" + nodeIP
	}

	// Environment variables — passed to exec directly, not via shell
	env := append(os.Environ(),
		"INSTALL_K3S_EXEC="+execArgs,
		"K3S_URL="+server,
		"K3S_TOKEN="+token,
	)
	if nodeName != "" {
		env = append(env, "K3S_NODE_NAME="+nodeName)
	}

	// Run sh with the script file path — NOT sh -c <string>
	// Arguments: sh /path/to/script
	cmd := exec.Command("sh", scriptPath)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// downloadInstallScript fetches the k3s installer to a temp file and returns the path.
func downloadInstallScript() (string, error) {
	resp, err := http.Get("https://get.k3s.io") //nolint:gosec // intentional HTTP GET for installer
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	f, err := os.CreateTemp("", "k3s-install-*.sh")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	if err := os.Chmod(f.Name(), 0o700); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	return f.Name(), nil
}

// checkConnectivity verifies the control plane is reachable using the k3s CA
// certificate from the kubeconfig. This avoids MITM risk from skipping TLS
// verification — the CA cert is the same one k3s uses to sign the server cert.
func checkConnectivity(serverURL string) error {
	tlsCfg, err := tlsConfigFromKubeconfig(*flagKubeConf)
	if err != nil {
		return fmt.Errorf("load k3s CA from kubeconfig: %w\n"+
			"  Ensure %s exists on the control plane node", err, *flagKubeConf)
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	resp, err := client.Get(serverURL + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// tlsConfigFromKubeconfig extracts the cluster CA certificate from a kubeconfig
// file and returns a *tls.Config that trusts only that CA. k3s embeds the CA as
// base64-encoded PEM under clusters[0].cluster.certificate-authority-data.
func tlsConfigFromKubeconfig(kubeconfigPath string) (*tls.Config, error) {
	data, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return nil, err
	}

	// Parse certificate-authority-data from the YAML manually.
	// Avoids pulling in a full YAML/k8s client dependency.
	var caB64 string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "certificate-authority-data:") {
			caB64 = strings.TrimSpace(strings.TrimPrefix(trimmed, "certificate-authority-data:"))
			break
		}
	}
	if caB64 == "" {
		return nil, fmt.Errorf("certificate-authority-data not found in %s", kubeconfigPath)
	}

	caDER, err := base64.StdEncoding.DecodeString(caB64)
	if err != nil {
		return nil, fmt.Errorf("decode CA cert: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caDER) {
		return nil, fmt.Errorf("could not add k3s CA to cert pool")
	}

	return &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}, nil
}

func waitForNodeReady(ctx context.Context, nodeName, kubeconfig string) (*NodeStatus, error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	fmt.Printf("  Polling")
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return nil, fmt.Errorf("timeout waiting for node %q", nodeName)
		case <-ticker.C:
			fmt.Printf(".")
			node, err := getNode(nodeName, kubeconfig)
			if err != nil {
				continue
			}
			for _, cond := range node.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					fmt.Println()
					return node, nil
				}
			}
		}
	}
}

func getNode(name, kubeconfig string) (*NodeStatus, error) {
	// Build args without shell — each element is a distinct argument.
	kubectlArgs := []string{"get", "node", name, "-o", "json", "--kubeconfig=" + kubeconfig}

	var out []byte
	var err error

	// Try k3s kubectl first, then fall back to standalone kubectl
	if _, lookErr := exec.LookPath("k3s"); lookErr == nil {
		out, err = exec.Command("k3s", append([]string{"kubectl"}, kubectlArgs...)...).Output()
	} else {
		out, err = exec.Command("kubectl", kubectlArgs...).Output()
	}
	if err != nil {
		return nil, err
	}

	var node NodeStatus
	if err := json.Unmarshal(out, &node); err != nil {
		return nil, err
	}
	return &node, nil
}

func buildNodeLabels(node *NodeStatus) map[string]string {
	labels := make(map[string]string)
	if node.Status.NodeInfo.Architecture == "arm64" {
		labels["hardware/arch"] = "arm64"
		labels["hardware/type"] = "raspberry-pi"
		if part := readCPUPart(); part == "0xd0b" {
			labels["hardware/cpu"] = "cortex-a76"
			labels["hardware/model"] = "rpi5"
		}
	} else if node.Status.NodeInfo.Architecture == "amd64" {
		labels["hardware/arch"] = "amd64"
	}
	return labels
}

func readCPUPart() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "CPU part") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func applyLabels(nodeName string, labels map[string]string, kubeconfig string) error {
	// Build label args as a slice — each key=value is a separate element, never shell-interpolated.
	kubectlArgs := []string{"label", "node", nodeName, "--overwrite", "--kubeconfig=" + kubeconfig}
	for k, v := range labels {
		// Labels are built from detected hardware values — safe, but we validate format.
		if validHostname.MatchString(strings.Split(k, "/")[len(strings.Split(k, "/"))-1]) {
			kubectlArgs = append(kubectlArgs, k+"="+v)
		}
	}

	var cmd *exec.Cmd
	if _, err := exec.LookPath("k3s"); err == nil {
		cmd = exec.Command("k3s", append([]string{"kubectl"}, kubectlArgs...)...)
	} else {
		cmd = exec.Command("kubectl", kubectlArgs...)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func serverHost(serverURL string) string {
	s := strings.TrimPrefix(serverURL, "https://")
	s = strings.TrimPrefix(s, "http://")
	// Strip port
	if idx := strings.LastIndex(s, ":"); idx > 0 {
		return s[:idx]
	}
	return s
}
