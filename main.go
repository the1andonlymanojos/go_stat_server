package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	Server ServerMetrics `json:"server"`
	//	Kubernetes KubernetesMetrics `json:"kubernetes"`
	TimeLeft int64 `json:"time_left"`
	//	Uptime     string            `json:"uptime"`
}

type ServerMetrics struct {
	LoadAvg        LoadAvg       `json:"load_avg"`
	Memory         MemoryStat    `json:"memory"`
	TotalDiskSpace float64       `json:"total_disk_space"`
	UsedDiskSpace  float64       `json:"used_disk_space"`
	NetworkStats   NetworkStats  `json:"network_stats"`
	UptimeMetrics  UptimeMetrics `json:"uptime_metrics"`
}

type MemoryStat struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type NetworkStats struct {
	TxRate  float64 `json:"tx_rate"`
	RxRate  float64 `json:"rx_rate"`
	TxBytes uint64  `json:"tx_bytes"`
	RxBytes uint64  `json:"rx_bytes"`
}

type KubernetesMetrics struct {
	NodeStatus string `json:"node_status"`
	PodStatus  string `json:"pod_status"`
	TopPods    string `json:"top_pods"`
	ErrorRates string `json:"error_rates"`
}

type UptimeMetrics struct {
	Uptime   string `json:"uptime"`
	IdleTime string `json:"idle_time"`
}

var (
	currentMetrics Metrics
	metricsMu      sync.RWMutex

	currentNetworkMetrics NetworkStats
	networkMu             sync.RWMutex

	currentUptimeMetrics UptimeMetrics
	uptimeMu             sync.RWMutex

	countdown int64
	mu        sync.Mutex

	uptimeFile = "uptime.log"
)

func main() {
	http.HandleFunc("/metrics", handleMetrics)
	http.HandleFunc("/shutdown", handleShutdown)
	fmt.Println(countdown)
	countdown = -1
	go startMetricsUpdater()
	go manageCountdown()
	go monitorNetworkSpeed(1*time.Second, "enp5s0")
	go monitorUptime(1 * time.Second)
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

	networkMu.RLock()
	networkStats := currentNetworkMetrics
	networkMu.RUnlock()

	uptimeMu.RLock()
	uptimeStats := currentUptimeMetrics
	uptimeMu.RUnlock()

	metrics.Server.NetworkStats = networkStats
	metrics.Server.UptimeMetrics = uptimeStats

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
	fmt.Println("Here 1")
	for range ticker.C {
		fmt.Println("Here 2")
		metrics, err := collectMetrics()
		if err != nil {
			fmt.Println("Failed to update metrics:", err)
			continue
		}
		fmt.Println(metrics)

		mu.Lock()
		metrics.TimeLeft = countdown
		mu.Unlock()

		metricsMu.Lock()
		currentMetrics = metrics
		metricsMu.Unlock()

	}
}

func collectMetrics() (Metrics, error) {
	serverMetrics, err := getServerMetrics()
	if err != nil {
		fmt.Println(err)
		return Metrics{}, err
	}

	//kubernetesMetrics, err := getKubernetesMetrics()

	return Metrics{
		Server: serverMetrics,
		//Kubernetes: kubernetesMetrics,
	}, nil
}

// LoadAvg represents the data from /proc/loadavg
type LoadAvg struct {
	Load1           float64 // 1-minute load average
	Load5           float64 // 5-minute load average
	Load15          float64 // 15-minute load average
	ActiveProcesses int     // Number of active (running) processes
	TotalProcesses  int     // Total number of processes
	LastPID         int     // Last created process ID
}

func ReadLoadAvg() (LoadAvg, error) {
	// Read the /proc/loadavg file
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		_ = fmt.Errorf("failed to read /proc/loadavg: %v", err)
	}

	// Split the content into fields
	fields := strings.Fields(string(data))
	if len(fields) < 5 {
		_ = fmt.Errorf("unexpected format in /proc/loadavg")
	}

	// Parse the load averages
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		_ = fmt.Errorf("failed to parse 1-minute load average: %v", err)
	}

	load5, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		_ = fmt.Errorf("failed to parse 5-minute load average: %v", err)
	}

	load15, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		_ = fmt.Errorf("failed to parse 15-minute load average: %v", err)
	}

	// Parse the active/total process count
	processInfo := strings.Split(fields[3], "/")
	if len(processInfo) != 2 {
		_ = fmt.Errorf("unexpected format for process info: %s", fields[3])
	}

	activeProcesses, err := strconv.Atoi(processInfo[0])
	if err != nil {
		_ = fmt.Errorf("failed to parse active process count: %v", err)
	}

	totalProcesses, err := strconv.Atoi(processInfo[1])
	if err != nil {
		_ = fmt.Errorf("failed to parse total process count: %v", err)
	}

	// Parse the last PID
	lastPID, err := strconv.Atoi(fields[4])
	if err != nil {
		_ = fmt.Errorf("failed to parse last PID: %v", err)
	}

	// Populate the LoadAvg struct
	loadAvg := LoadAvg{
		Load1:           load1,
		Load5:           load5,
		Load15:          load15,
		ActiveProcesses: activeProcesses,
		TotalProcesses:  totalProcesses,
		LastPID:         lastPID,
	}

	return loadAvg, nil
}
func getServerMetrics() (ServerMetrics, error) {
	loadAvg, err := ReadLoadAvg()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}

	memory, err := mem.VirtualMemory()

	// Actual used memory = Used - Cached - Buffers
	actualUsedMemory := float64(memory.Used) - float64(memory.Cached) - float64(memory.Buffers)

	// Convert memory to GB
	actualUsedMemoryGB := actualUsedMemory / (1024 * 1024 * 1024)     // Convert bytes to GB
	availableMemoryGB := float64(memory.Total) / (1024 * 1024 * 1024) // Convert bytes to GB

	// Print the stats
	fmt.Printf("Actual Used Memory: %.2f GB\n", actualUsedMemoryGB)
	fmt.Printf("Available Memory: %.2f GB\n", availableMemoryGB)
	if err != nil {
		log.Printf("Error getting memory stats: %v", err)
	}
	disks := []string{"/", "/srv/nfs/shared", "/mnt/vault"}
	totalUsed := 0.0
	totalDiskSpace := 0.0
	for _, drive := range disks {
		diskUsage, _ := disk.Usage(drive)
		totalUsed += float64(diskUsage.Used) / (1024 * 1024 * 1024)       // Convert bytes to GB
		totalDiskSpace += float64(diskUsage.Total) / (1024 * 1024 * 1024) // Convert bytes to GB

	}
	//diskUsage1, err := disk.Usage("/srv/nfs/shared")
	////find total and used disk space
	//totalDiskSpace := float64(diskUsage1.Total) / (1024 * 1024 * 1024) // Convert bytes to GB
	//usedDiskSpace := float64(diskUsage1.Used) / (1024 * 1024 * 1024)   // Convert bytes to GB
	fmt.Println("Total disk space: ", totalDiskSpace)
	fmt.Println("Used disk space: ", totalUsed)
	if err != nil {
		log.Printf("Error getting disk usage: %v", err)
	}

	fmt.Println("Here 3")
	metrics := ServerMetrics{}
	metrics.TotalDiskSpace = totalDiskSpace
	metrics.UsedDiskSpace = totalUsed
	metrics.Memory = MemoryStat{
		Total: uint64(availableMemoryGB),
		Used:  uint64(actualUsedMemoryGB),
	}
	metrics.LoadAvg = loadAvg
	return metrics, nil
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

func monitorNetworkSpeed(interval time.Duration, interfaceName string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	writeInterval := 5
	var prevTx, prevRx uint64
	var cumulativeTx, cumulativeRx float64
	counter := 0

	// Load existing totals from the log file
	file := "network.log"
	if data, err := os.ReadFile(file); err == nil {
		lines := strings.Split(string(data), "\n")
		if len(lines) > 1 {
			timestamp := ""
			fmt.Sscanf(lines[len(lines)-2], "%s - Tx: %f MB, Rx: %f MB", &timestamp, &cumulativeTx, &cumulativeRx)

			fmt.Println(lines[len(lines)-2])

			fmt.Println("found: ", cumulativeTx, cumulativeRx)
		}
	}

	for {
		select {
		case <-ticker.C:
			// Get current stats for the specified interface
			stats, err := net.IOCounters(true)
			if err != nil {
				log.Printf("Error getting network stats: %v", err)
				continue
			}

			var currentTx, currentRx uint64
			for _, stat := range stats {
				if stat.Name == interfaceName {
					currentTx = stat.BytesSent
					currentRx = stat.BytesRecv
					break
				}
			}

			if currentTx == 0 && currentRx == 0 {
				log.Printf("No stats found for interface: %s", interfaceName)
				continue
			}

			//Calculate and display current rates if previous values exist
			if prevTx > 0 && prevRx > 0 {
				txRate := float64(currentTx-prevTx) / interval.Seconds() * 8 / 1024 / 1024 // Mbps
				rxRate := float64(currentRx-prevRx) / interval.Seconds() * 8 / 1024 / 1024 // Mbps
				// Update cumulative totals
				cumulativeTx += float64(currentTx-prevTx) / 1024 / 1024 //in MB
				cumulativeRx += float64(currentRx-prevRx) / 1024 / 1024 //in MB
				temp := NetworkStats{
					TxRate:  txRate,
					RxRate:  rxRate,
					TxBytes: currentTx,
					RxBytes: currentRx,
				}

				networkMu.Lock()
				currentNetworkMetrics = temp
				networkMu.Unlock()

				counter++
				fmt.Println(txRate, rxRate)
			}

			if counter >= writeInterval {
				// Save updated totals to the log file
				timestamp := time.Now().Format(time.RFC3339)
				entry := fmt.Sprintf("%s - Tx: %.2f MB, Rx: %.2f MB\n", timestamp, cumulativeTx, cumulativeRx)
				fmt.Println(entry)
				err = os.WriteFile(file, []byte(entry), 0644)
				if err != nil {
					log.Printf("Error writing to file: %v", err)
				}
				counter = 0
			}

			// Update previous stats
			prevTx = currentTx
			prevRx = currentRx
		}
	}
}

func monitorUptime(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	writeInterval := 5
	counter := 0

	// Load existing uptime and idle time from the log file
	file := "uptime.log"
	var cumulativeUptime, cumulativeIdleTime float64
	if data, err := os.ReadFile(file); err == nil {
		lines := strings.Split(string(data), "\n")
		if len(lines) > 1 {
			timestamp := ""
			fmt.Sscanf(lines[len(lines)-2], "%s - Uptime: %f seconds, Idle: %f seconds", &timestamp, &cumulativeUptime, &cumulativeIdleTime)
			fmt.Println(lines[len(lines)-2])
			fmt.Println("found: ", cumulativeUptime, cumulativeIdleTime)
		}
	}

	var prevUptime, prevIdleTime float64

	for {
		select {
		case <-ticker.C:
			// Read uptime and idle time from /proc/uptime
			data, err := os.ReadFile("/proc/uptime")
			if err != nil {
				log.Printf("Error reading /proc/uptime: %v", err)
				continue
			}

			var uptime, idleTime float64
			fmt.Sscanf(string(data), "%f %f", &uptime, &idleTime)

			// Update cumulative uptime and idle time by the delta
			if prevUptime > 0 && prevIdleTime > 0 {
				cumulativeUptime += uptime - prevUptime
				cumulativeIdleTime += idleTime - prevIdleTime
			}

			// Update previous readings
			prevUptime = uptime
			prevIdleTime = idleTime

			counter++
			fmt.Println("Uptime:", uptime, "Idle:", idleTime)

			uptimeMu.Lock()
			currentUptimeMetrics = UptimeMetrics{
				Uptime:   fmt.Sprintf("%.2f seconds", cumulativeUptime),
				IdleTime: fmt.Sprintf("%.2f seconds", cumulativeIdleTime),
			}
			uptimeMu.Unlock()

			if counter >= writeInterval {
				// Save updated uptime and idle time to the log file
				timestamp := time.Now().Format(time.RFC3339)
				entry := fmt.Sprintf("%s - Uptime: %.2f seconds, Idle: %.2f seconds\n", timestamp, cumulativeUptime, cumulativeIdleTime)
				fmt.Println(entry)
				err = os.WriteFile(file, []byte(entry), 0644)
				if err != nil {
					log.Printf("Error writing to file: %v", err)
				}
				counter = 0
			}
		}
	}
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
