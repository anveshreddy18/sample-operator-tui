package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Pod represents a simplified view of a Kubernetes pod for our list
type Pod struct {
	Name      string
	Namespace string
	Status    string
	Ready     string
	Age       string
	Node      string
}

// Implement the list.Item interface for bubbletea list component
func (p Pod) FilterValue() string { return p.Name }
func (p Pod) Title() string       { return p.Name }
func (p Pod) Description() string {
	return fmt.Sprintf("Status: %s | Ready: %s | Node: %s | Age: %s",
		p.Status, p.Ready, p.Node, p.Age)
}

// AppState represents the different screens our TUI can be in
type AppState int

const (
	ListState AppState = iota
	LogState
	DescribeState
	ContainerSelectState // New state for selecting a container
	YamlState
)

// Model holds our application state
type Model struct {
	state         AppState
	list          list.Model
	viewport      viewport.Model
	pods          []Pod
	selectedPod   Pod
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface
	namespace     string
	etcdName      string
	content       string
	err           error
	containerList list.Model // List for containers in a pod
	containers    []string   // Names of containers in the selected pod
}

// Kubernetes client setup - this is where we establish connection to the cluster
func setupKubeClient() (kubernetes.Interface, dynamic.Interface, error) {
	// Use kubeconfig from KUBECONFIG env var or default location (~/.kube/config)
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	// Create the standard Kubernetes client for basic operations
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Create dynamic client for working with Custom Resources
	// This is essential because Etcd is a CRD, not a built-in Kubernetes type
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	return kubeClient, dynamicClient, nil
}

// fetchEtcdResource retrieves the Etcd custom resource
// This demonstrates how to work with CRDs using the dynamic client
func (m *Model) fetchEtcdResource() (*unstructured.Unstructured, error) {
	// Define the Group, Version, Resource for Etcd CRD
	// This is the schema identifier for the custom resource
	etcdGVR := schema.GroupVersionResource{
		Group:    "druid.gardener.cloud",
		Version:  "v1alpha1",
		Resource: "etcds",
	}

	// Fetch the Etcd resource from the specified namespace
	etcdResource, err := m.dynamicClient.Resource(etcdGVR).
		Namespace(m.namespace).
		Get(context.Background(), m.etcdName, metav1.GetOptions{})

	if err != nil {
		return nil, fmt.Errorf("failed to get Etcd resource %s/%s: %w",
			m.namespace, m.etcdName, err)
	}

	return etcdResource, nil
}

// fetchEtcdPods retrieves pods managed by the StatefulSet that corresponds to our Etcd resource
func (m *Model) fetchEtcdPods() ([]Pod, error) {
	// The key insight here is that etcd-druid creates a StatefulSet with the same name as the Etcd resource
	// We use label selectors to find pods managed by this StatefulSet
	labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s", m.etcdName)

	podList, err := m.kubeClient.CoreV1().Pods(m.namespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: labelSelector})

	if err != nil {
		return nil, fmt.Errorf("failed to list etcd pods: %w", err)
	}

	var pods []Pod
	for _, pod := range podList.Items {
		// Calculate pod age - this gives users context about pod lifecycle
		age := time.Since(pod.CreationTimestamp.Time).Truncate(time.Second)

		// Determine ready status by checking container readiness
		readyCount := 0
		totalCount := len(pod.Status.ContainerStatuses)
		for _, status := range pod.Status.ContainerStatuses {
			if status.Ready {
				readyCount++
			}
		}

		pods = append(pods, Pod{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			Status:    string(pod.Status.Phase),
			Ready:     fmt.Sprintf("%d/%d", readyCount, totalCount),
			Age:       age.String(),
			Node:      pod.Spec.NodeName,
		})
	}

	return pods, nil
}

// fetchPodContainers retrieves the list of containers for a given pod
func (m *Model) fetchPodContainers(podName string) ([]string, error) {
	pod, err := m.kubeClient.CoreV1().Pods(m.namespace).Get(
		context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s: %w", podName, err)
	}
	var containers []string
	for _, c := range pod.Spec.Containers {
		containers = append(containers, c.Name)
	}
	return containers, nil
}

// fetchPodYAML retrieves the YAML configuration for a given pod
func (m *Model) fetchPodYAML(podName string) (string, error) {
	pod, err := m.kubeClient.CoreV1().Pods(m.namespace).Get(
		context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get pod %s: %w", podName, err)
	}
	b, err := yaml.Marshal(pod)
	if err != nil {
		return "", fmt.Errorf("failed to marshal pod to yaml: %w", err)
	}
	return string(b), nil
}

// getPodLogs retrieves logs for the selected pod and container
func (m *Model) getPodLogs(podName, container string) (string, error) {
	// Configure log retrieval options
	// TailLines limits output to prevent overwhelming the terminal
	tailLines := int64(100)
	req := m.kubeClient.CoreV1().Pods(m.namespace).GetLogs(podName, &corev1.PodLogOptions{
		TailLines: &tailLines,
		Container: container,
	})

	// Execute the request and read the response
	logs, err := req.Stream(context.Background())
	if err != nil {
		return "", fmt.Errorf("failed to get logs for pod %s (container %s): %w", podName, container, err)
	}
	defer logs.Close()

	// Read all log content
	buf := make([]byte, 2048)
	var result strings.Builder
	for {
		n, err := logs.Read(buf)
		if n > 0 {
			result.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}

	return result.String(), nil
}

// describePod gets detailed information about a pod
// This mimics the 'kubectl describe pod' functionality
func (m *Model) describePod(podName string) (string, error) {
	pod, err := m.kubeClient.CoreV1().Pods(m.namespace).Get(
		context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to describe pod %s: %w", podName, err)
	}

	// Build a description similar to kubectl describe
	var desc strings.Builder
	desc.WriteString(fmt.Sprintf("Name: %s\n", pod.Name))
	desc.WriteString(fmt.Sprintf("Namespace: %s\n", pod.Namespace))
	desc.WriteString(fmt.Sprintf("Node: %s\n", pod.Spec.NodeName))
	desc.WriteString(fmt.Sprintf("Status: %s\n", pod.Status.Phase))
	desc.WriteString(fmt.Sprintf("IP: %s\n", pod.Status.PodIP))
	desc.WriteString(fmt.Sprintf("Created: %s\n", pod.CreationTimestamp.Time.Format(time.RFC3339)))

	desc.WriteString("\nContainers:\n")
	for _, container := range pod.Spec.Containers {
		desc.WriteString(fmt.Sprintf("  %s: %s\n", container.Name, container.Image))
	}

	desc.WriteString("\nConditions:\n")
	for _, condition := range pod.Status.Conditions {
		desc.WriteString(fmt.Sprintf("  %s: %s\n", condition.Type, condition.Status))
	}

	return desc.String(), nil
}

// Initialize sets up the initial state of our application
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.list.StartSpinner(),
		m.loadPods(),
	)
}

// loadPods is a command that fetches pod data asynchronously
func (m *Model) loadPods() tea.Cmd {
	return func() tea.Msg {
		pods, err := m.fetchEtcdPods()
		if err != nil {
			return errMsg{err}
		}
		return podsLoadedMsg{pods}
	}
}

// Message types for the Elm architecture pattern used by bubbletea
type podsLoadedMsg struct{ pods []Pod }
type errMsg struct{ err error }
type logsLoadedMsg struct{ content string }
type describeLoadedMsg struct{ content string }
type containersLoadedMsg struct{ containers []string }
type containerSelectedMsg struct{ container string }
type yamlLoadedMsg struct{ content string }

// Update handles all state changes in response to messages
// This is the heart of the Elm architecture - pure function that transforms state
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if msg.String() == "r" {
			switch m.state {
			case ListState:
				return m, m.loadPods()
			case LogState:
				if m.selectedPod.Name != "" && m.content != "" {
					container := ""
					if len(m.containers) > 0 && m.containerList.Index() >= 0 && m.containerList.Index() < len(m.containers) {
						container = m.containers[m.containerList.Index()]
					}
					if container == "" {
						container = m.selectedPod.Name // fallback, but should not happen
					}
					return m, func() tea.Msg {
						content, err := m.getPodLogs(m.selectedPod.Name, container)
						if err != nil {
							return errMsg{err}
						}
						return logsLoadedMsg{content}
					}
				}
			case DescribeState:
				if m.selectedPod.Name != "" {
					return m, func() tea.Msg {
						content, err := m.describePod(m.selectedPod.Name)
						if err != nil {
							return errMsg{err}
						}
						return describeLoadedMsg{content}
					}
				}
			case ContainerSelectState:
				if m.selectedPod.Name != "" {
					return m, func() tea.Msg {
						containers, err := m.fetchPodContainers(m.selectedPod.Name)
						if err != nil {
							return errMsg{err}
						}
						return containersLoadedMsg{containers}
					}
				}
			}
		}
		switch m.state {
		case ListState:
			switch msg.String() {
			case "q":
				return m, tea.Quit
			case "l":
				// Load containers for selected pod and show container selection
				if len(m.pods) > 0 {
					m.selectedPod = m.pods[m.list.Index()]
					m.state = ContainerSelectState
					return m, func() tea.Msg {
						containers, err := m.fetchPodContainers(m.selectedPod.Name)
						if err != nil {
							return errMsg{err}
						}
						return containersLoadedMsg{containers}
					}
				}
			case "d":
				// Describe selected pod
				if len(m.pods) > 0 {
					m.selectedPod = m.pods[m.list.Index()]
					m.state = DescribeState
					return m, func() tea.Msg {
						content, err := m.describePod(m.selectedPod.Name)
						if err != nil {
							return errMsg{err}
						}
						return describeLoadedMsg{content}
					}
				}
			case "r":
				// Refresh pod list
				return m, m.loadPods()
			case "y":
				// Show YAML for selected pod
				if len(m.pods) > 0 {
					m.selectedPod = m.pods[m.list.Index()]
					m.state = YamlState
					return m, func() tea.Msg {
						content, err := m.fetchPodYAML(m.selectedPod.Name)
						if err != nil {
							return errMsg{err}
						}
						return yamlLoadedMsg{content}
					}
				}
			default:
				m.list, cmd = m.list.Update(msg)
				cmds = append(cmds, cmd)
			}
		case LogState:
			switch msg.String() {
			case "q":
				m.state = ListState
				m.content = ""
			case "esc":
				m.state = ContainerSelectState
				m.content = ""
			default:
				m.viewport, cmd = m.viewport.Update(msg)
				cmds = append(cmds, cmd)
			}
		case DescribeState:
			switch msg.String() {
			case "q", "esc":
				m.state = ListState
				m.content = ""
			default:
				m.viewport, cmd = m.viewport.Update(msg)
				cmds = append(cmds, cmd)
			}
		case ContainerSelectState:
			switch msg.String() {
			case "q", "esc":
				m.state = ListState
				return m, nil
			case "enter":
				if len(m.containers) > 0 {
					selected := m.containers[m.containerList.Index()]
					return m, func() tea.Msg {
						return containerSelectedMsg{container: selected}
					}
				}
			default:
				m.containerList, cmd = m.containerList.Update(msg)
				cmds = append(cmds, cmd)
			}
		case YamlState:
			switch msg.String() {
			case "q", "esc":
				m.state = ListState
				m.content = ""
			default:
				m.viewport, cmd = m.viewport.Update(msg)
				cmds = append(cmds, cmd)
			}
		}

	case podsLoadedMsg:
		m.pods = msg.pods
		// Convert pods to list items for the bubbletea list component
		items := make([]list.Item, len(m.pods))
		for i, pod := range m.pods {
			items[i] = pod
		}
		m.list.SetItems(items)
		m.list.StopSpinner()

	case logsLoadedMsg:
		m.content = msg.content
		m.viewport.SetContent(m.content)
		m.state = LogState
		return m, nil

	case describeLoadedMsg:
		m.content = msg.content
		m.viewport.SetContent(m.content)

	case containersLoadedMsg:
		m.containers = msg.containers
		items := make([]list.Item, len(msg.containers))
		for i, c := range msg.containers {
			items[i] = listItemString(c)
		}
		delegate := list.NewDefaultDelegate()
		containerList := list.New(items, delegate, m.list.Width(), m.list.Height())
		containerList.Title = "Containers"
		containerList.SetShowStatusBar(false)
		containerList.SetFilteringEnabled(false)
		containerList.SetShowHelp(false)
		m.containerList = containerList
		m.state = ContainerSelectState
		return m, nil

	case containerSelectedMsg:
		if m.selectedPod.Name != "" && msg.container != "" {
			return m, func() tea.Msg {
				content, err := m.getPodLogs(m.selectedPod.Name, msg.container)
				if err != nil {
					return errMsg{err}
				}
				return logsLoadedMsg{content}
			}
		}
		return m, nil

	case yamlLoadedMsg:
		m.content = msg.content
		m.viewport.SetContent(m.content)
		m.state = YamlState

	case errMsg:
		m.err = msg.err
		m.list.StopSpinner()

	case tea.WindowSizeMsg:
		// Handle terminal resizing gracefully
		m.list.SetWidth(msg.Width)
		m.list.SetHeight(msg.Height - 4)
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - 4
	}

	return m, tea.Batch(cmds...)
}

// View renders the current state of the application
// This separates presentation logic from business logic
func (m Model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\nPress 'q' to quit.", m.err)
	}

	switch m.state {
	case ListState:
		header := headerStyle.Render(fmt.Sprintf("Etcd Pods (%s/%s)", m.namespace, m.etcdName))
		help := helpStyle.Render("• l: logs • d: describe • r: refresh • q: quit")
		return fmt.Sprintf("%s\n%s\n%s", header, m.list.View(), help)

	case LogState:
		header := headerStyle.Render(fmt.Sprintf("Logs: %s", m.selectedPod.Name))
		help := helpStyle.Render("• esc: back • q: quit • ↑/↓: scroll")
		return fmt.Sprintf("%s\n%s\n%s", header, m.viewport.View(), help)

	case DescribeState:
		header := headerStyle.Render(fmt.Sprintf("Describe: %s", m.selectedPod.Name))
		help := helpStyle.Render("• esc: back • q: quit • ↑/↓: scroll")
		return fmt.Sprintf("%s\n%s\n%s", header, m.viewport.View(), help)

	case ContainerSelectState:
		header := headerStyle.Render(fmt.Sprintf("Select Container: %s", m.selectedPod.Name))
		help := helpStyle.Render("• enter: select • esc: back • q: quit")
		return fmt.Sprintf("%s\n%s\n%s", header, m.containerList.View(), help)

	case YamlState:
		header := headerStyle.Render(fmt.Sprintf("YAML Config: %s", m.selectedPod.Name))
		help := helpStyle.Render("• esc: back • q: quit • ↑/↓: scroll")
		return fmt.Sprintf("%s\n%s\n%s", header, m.viewport.View(), help)
	}

	return ""
}

// Styling using lipgloss - this makes our TUI visually appealing
var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212")).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			MarginBottom(1)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			MarginTop(1)
)

// listItemString wraps a string to implement the list.Item interface for Bubbletea lists
// Used for displaying container names in the container selection list

type listItemString string

func (s listItemString) Title() string       { return string(s) }
func (s listItemString) Description() string { return "" }
func (s listItemString) FilterValue() string { return string(s) }

func main() {
	// Parse command line arguments - k9s passes context information this way
	if len(os.Args) < 3 {
		log.Fatal("Usage: etcd-pod-viewer <namespace> <etcd-name>")
	}

	namespace := os.Args[1]
	etcdName := os.Args[2]

	// Initialize Kubernetes clients
	kubeClient, dynamicClient, err := setupKubeClient()
	if err != nil {
		log.Fatalf("Failed to setup kubernetes client: %v", err)
	}

	// Create the list component with custom styling
	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(lipgloss.Color("212")).Bold(true)

	podList := list.New([]list.Item{}, delegate, 0, 0)
	podList.Title = "Etcd Pods"
	podList.SetShowStatusBar(false)
	podList.SetFilteringEnabled(false)
	podList.SetShowHelp(false)

	// Create viewport for displaying logs and descriptions
	vp := viewport.New(80, 20)

	// Initialize our model
	model := Model{
		state:         ListState,
		list:          podList,
		viewport:      vp,
		kubeClient:    kubeClient,
		dynamicClient: dynamicClient,
		namespace:     namespace,
		etcdName:      etcdName,
	}

	// Start the bubbletea program
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatalf("Error running program: %v", err)
	}
}
