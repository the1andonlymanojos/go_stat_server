package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/net"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Metrics struct {
	Server     ServerMetrics     `json:"server"`
	Kubernetes KubernetesMetrics `json:"kubernetes"`
	TimeLeft   int64             `json:"time_left"`
	Uptime     string            `json:"uptime"`
}

type ServerMetrics struct {
	CPUUsage []float64              `json:"cpu_usage"`
	Memory   *mem.VirtualMemoryStat `json:"memory"`
	Disk     *disk.UsageStat        `json:"disk"`
	Network  []net.IOCountersStat   `json:"network"`
}

type KubernetesMetrics struct {
	NodeStatus string `json:"node_status"`
	PodStatus  string `json:"pod_status"`
	TopPods    string `json:"top_pods"`
	ErrorRates string `json:"error_rates"`
}

var (
	currentMetrics Metrics
	metricsMu      sync.RWMutex

	countdown int64
	mu        sync.Mutex

	totalUptimeSeconds int64
	uptimeFile         = "uptime.log"
)

func main() {
	http.HandleFunc("/metrics", handleMetrics)
	http.HandleFunc("/shutdown", handleShutdown)
	fmt.Println(countdown)
	countdown = -1
	loadUptime()
	go startMetricsUpdater()
	go manageCountdown()

	go func() {
		log.Println("Starting pprof server on :6060")
		log.Fatal(http.ListenAndServe("localhost:6060", nil)) // pprof will be available here
	}()

	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		fmt.Println(err)
	}
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	metricsMu.RLock()
	metrics := currentMetrics
	metricsMu.RUnlock()

	encodedMetrics := encodeMetrics(metrics)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(encodedMetrics))
}

func handleShutdown(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		query := r.URL.Query()
		timeStr := query.Get("time")
		if timeStr == "" {
			http.Error(w, "Missing 'time' parameter", http.StatusBadRequest)
			return
		}

		seconds, err := strconv.ParseInt(timeStr, 10, 64)
		if err != nil {
			http.Error(w, "Invalid 'time' parameter", http.StatusBadRequest)
			return
		}

		mu.Lock()
		countdown = seconds
		mu.Unlock()
		fmt.Fprintf(w, "Countdown started/updated to %d seconds", seconds)

	case http.MethodDelete:
		mu.Lock()
		countdown = -1
		mu.Unlock()
		fmt.Fprintln(w, "Countdown cancelled")

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func startMetricsUpdater() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		metrics, err := collectMetrics()
		if err != nil {
			fmt.Println("Failed to update metrics:", err)
			continue
		}

		mu.Lock()
		metrics.TimeLeft = countdown
		mu.Unlock()

		metricsMu.Lock()
		currentMetrics = metrics
		metricsMu.Unlock()

		incrementUptime()
	}
}

func collectMetrics() (Metrics, error) {
	serverMetrics, err := getServerMetrics()
	if err != nil {
		return Metrics{}, err
	}

	kubernetesMetrics, err := getKubernetesMetrics()
	if err != nil {
		return Metrics{}, err
	}

	return Metrics{
		Server:     serverMetrics,
		Kubernetes: kubernetesMetrics,
		Uptime:     fmt.Sprintf("%d seconds", totalUptimeSeconds),
	}, nil
}

func getServerMetrics() (ServerMetrics, error) {
	cpuUsage, err := cpu.Percent(1, false)
	if err != nil {
		log.Printf("Error getting CPU usage: %v", err)
	}

	memory, err := mem.VirtualMemory()
	if err != nil {
		log.Printf("Error getting memory stats: %v", err)
	}

	diskUsage, err := disk.Usage("/srv/nfs/shared")
	if err != nil {
		log.Printf("Error getting disk usage: %v", err)
	}

	network, err := net.IOCounters(false)
	if err != nil {
		log.Printf("Error getting network stats: %v", err)
	}
	metrics := ServerMetrics{}
	metrics.Disk = diskUsage
	metrics.Memory = memory
	metrics.CPUUsage = cpuUsage
	metrics.Network = network
	return metrics, nil
}

func getNetworkBytes() (uint64, uint64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}

	var txBytes, rxBytes uint64
	lines := strings.Split(string(data), "\n")

	for _, line := range lines[2:] {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[1], 10, 64)
		tx, _ := strconv.ParseUint(fields[9], 10, 64)
		rxBytes += rx
		txBytes += tx
	}

	return txBytes, rxBytes
}

func loadUptime() {
	data, err := os.ReadFile(uptimeFile)
	if err == nil {
		totalUptimeSeconds, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	}
}

func incrementUptime() {
	totalUptimeSeconds++
	if totalUptimeSeconds%10 == 0 { // Save uptime every 10 seconds
		err := os.WriteFile(uptimeFile, []byte(fmt.Sprintf("%d", totalUptimeSeconds)), 0644)
		if err != nil {
			return
		}
	}
}

func manageCountdown() {
	for {
		time.Sleep(10000 * time.Second)
		mu.Lock()
		if countdown == -1 {
			mu.Unlock()
			fmt.Println("Skipping")
			continue
		}

		if countdown > 0 {
			countdown--
		} else if countdown == 0 {
			broadcastMessage("System shutting down now!")
			performShutdown()
		}
		mu.Unlock()
	}
}

func broadcastMessage(message string) {
	cmd := fmt.Sprintf("echo '%s' | wall", message)
	_, err := runCommand(cmd)
	if err != nil {
		fmt.Println("Failed to broadcast message:", err)
	}
}

func performShutdown() {
	cmd := "shutdown -h now"
	_, err := runCommand(cmd)
	if err != nil {
		fmt.Println("Failed to perform shutdown:", err)
	}
}

func runCommand(cmd string) (string, error) {
	output, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func encodeMetrics(metrics Metrics) string {
	jsonData, err := json.Marshal(metrics)
	if err != nil {
		fmt.Println("Error marshalling metrics:", err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(jsonData)
}

//	func handleMetrics(w http.ResponseWriter, r *http.Request) {
//		metrics, err := collectMetrics()
//		if err != nil {
//			http.Error(w, "Failed to collect metrics", http.StatusInternalServerError)
//			return
//		}
//
//		encodedMetrics := encodeMetrics(metrics)
//		w.Header().Set("Content-Type", "application/json")
//		w.Write([]byte(encodedMetrics))
//	}
//
//	func handleShutdown(w http.ResponseWriter, r *http.Request) {
//		switch r.Method {
//		case http.MethodPost:
//			// Start or update countdown
//			query := r.URL.Query()
//			timeStr := query.Get("time")
//			if timeStr == "" {
//				http.Error(w, "Missing 'time' parameter", http.StatusBadRequest)
//				return
//			}
//
//			seconds, err := strconv.ParseInt(timeStr, 10, 64)
//			if err != nil {
//				http.Error(w, "Invalid 'time' parameter", http.StatusBadRequest)
//				return
//			}
//
//			mu.Lock()
//			countdown = seconds
//			mu.Unlock()
//			fmt.Fprintf(w, "Countdown started/updated to %d seconds", seconds)
//		case http.MethodDelete:
//			// Cancel countdown
//			mu.Lock()
//			countdown = 0
//			mu.Unlock()
//			fmt.Fprintln(w, "Countdown cancelled")
//		default:
//			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
//		}
//	}
//
//	func manageCountdown() {
//		for {
//			mu.Lock()
//			if countdown > 0 {
//				switch countdown {
//				case 600, 300, 60:
//					message := fmt.Sprintf("System will shut down in %d minutes!", countdown/60)
//					broadcastMessage(message)
//				case 0:
//					broadcastMessage("System shutting down now!")
//					performShutdown()
//				}
//				countdown--
//			}
//			mu.Unlock()
//			time.Sleep(1 * time.Second)
//		}
//	}
//
//	func broadcastMessage(message string) {
//		cmd := fmt.Sprintf("echo '%s' | wall", message)
//		_, err := runCommand(cmd)
//		if err != nil {
//			fmt.Println("Failed to broadcast message:", err)
//		}
//	}
//
//	func performShutdown() {
//		cmd := "shutdown -h now"
//		_, err := runCommand(cmd)
//		if err != nil {
//			fmt.Println("Failed to perform shutdown:", err)
//		}
//	}
//
//	func collectMetrics() (Metrics, error) {
//		serverMetrics, err := getServerMetrics()
//		if err != nil {
//			return Metrics{}, err
//		}
//
//		kubernetesMetrics, err := getKubernetesMetrics()
//		if err != nil {
//			return Metrics{}, err
//		}
//
//		return Metrics{
//			Server:     serverMetrics,
//			Kubernetes: kubernetesMetrics,
//		}, nil
//	}
//
//	func getServerMetrics() (ServerMetrics, error) {
//		cpuUsage, err := runCommand("mpstat -P ALL 1 1 | awk '/Average/ {print $3}'")
//		if err != nil {
//			return ServerMetrics{}, err
//		}
//
//		memoryUsage, err := runCommand("free -h | awk '/Mem:/ {print $3 \" / \" $2}'")
//		if err != nil {
//			return ServerMetrics{}, err
//		}
//
//		diskUsage, err := runCommand("df -h --output=source,size,used,avail,pcent | tail -n +2")
//		if err != nil {
//			return ServerMetrics{}, err
//		}
//
//		networkStats, err := runCommand("ifstat -i eth0 1 1 | awk 'NR==3 {print $1, $2}'")
//		if err != nil {
//			return ServerMetrics{}, err
//		}
//
//		loggedInUsers, err := runCommand("who | wc -l")
//		if err != nil {
//			return ServerMetrics{}, err
//		}
//
//		loggedInUsersInt := 0
//		fmt.Sscanf(strings.TrimSpace(loggedInUsers), "%d", &loggedInUsersInt)
//
//		return ServerMetrics{
//			CPUUsage:      strings.TrimSpace(cpuUsage),
//			MemoryUsage:   strings.TrimSpace(memoryUsage),
//			DiskUsage:     strings.Split(strings.TrimSpace(diskUsage), "\n"),
//			NetworkUsage:  strings.TrimSpace(networkStats),
//			LoggedInUsers: loggedInUsersInt,
//		}, nil
//	}
func getKubernetesMetrics() (KubernetesMetrics, error) {
	nodeStatus, _ := runCommand("kubectl get nodes --no-headers | awk '{print $1 \" \" $2}'")
	podStatus, _ := runCommand("kubectl get pods --all-namespaces --no-headers | awk '{print $4}' | sort | uniq -c")
	topPods, _ := runCommand("kubectl top pods --all-namespaces --containers | sort -k3 -nr | head -5")
	errorRates, _ := runCommand("kubectl logs --all-namespaces | grep 'error' | wc -l")

	return KubernetesMetrics{
		NodeStatus: strings.TrimSpace(nodeStatus),
		PodStatus:  strings.TrimSpace(podStatus),
		TopPods:    strings.TrimSpace(topPods),
		ErrorRates: strings.TrimSpace(errorRates),
	}, nil
}

//
//func encodeMetrics(metrics Metrics) string {
//	jsonData, err := json.Marshal(metrics)
//	if err != nil {
//		fmt.Println("Error marshalling metrics:", err)
//		return ""
//	}
//	return base64.StdEncoding.EncodeToString(jsonData)
//}
//
//func runCommand(cmd string) (string, error) {
//	output, err := exec.Command("bash", "-c", cmd).Output()
//	if err != nil {
//		return "", err
//	}
//	return string(output), nil
//}
