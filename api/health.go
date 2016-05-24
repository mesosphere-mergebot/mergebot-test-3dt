package api

import (
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/coreos/go-systemd/dbus"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// unitsHealthReport gets updated in a separate goroutine every 60 sec This variable contains a status of a DC/OS
// systemd units for the past 60 sec.
var unitsHealthReport unitsHealth

// unitsHealth is a container for global health report. Getting and updating of health report must go through
// GetHealthReport and UpdateHealthReport this allows to access data in concurrent manner.
type unitsHealth struct {
	sync.Mutex
	healthReport UnitsHealthResponseJSONStruct
}

// getHealthReport returns a global health report of UnitsHealthResponseJsonStruct type.
func (uh *unitsHealth) GetHealthReport() UnitsHealthResponseJSONStruct {
	uh.Lock()
	defer uh.Unlock()
	return uh.healthReport
}

// updateHealthReport updates a global health report of UnitsHealthResponseJsonStruct type.
func (uh *unitsHealth) UpdateHealthReport(healthReport UnitsHealthResponseJSONStruct) {
	uh.Lock()
	defer uh.Unlock()
	uh.healthReport = healthReport
}

// StartUpdateHealthReport should be started in a separate goroutine to update global health report periodically.
func StartUpdateHealthReport(config Config, readyChan chan struct{}, runOnce bool) {
	var closedReadyChan bool
	for {
		healthReport, err := GetUnitsProperties(&config)
		if err == nil {
			if !closedReadyChan {
				close(readyChan)
				closedReadyChan = true
			}
		} else {
			log.Error("Could not update systemd units health report")
			log.Error(err)
		}
		unitsHealthReport.UpdateHealthReport(healthReport)
		if runOnce {
			log.Debug("Run startUpdateHealthReport only once")
			return
		}
		time.Sleep(time.Duration(config.FlagUpdateHealthReportInterval) * time.Second)
	}
}

// dcosTools is implementation of DCOSHelper interface.
type dcosTools struct {
	sync.Mutex
	dcon     *dbus.Conn
	hostname string
	role     string
	ip       string
	mesosID  string
}

// GetHostname return a localhost hostname.
func (st *dcosTools) GetHostname() (string, error) {
	if st.hostname != "" {
		return st.hostname, nil
	}
	var err error
	st.hostname, err = os.Hostname()
	if err != nil {
		return "", err
	}
	return st.hostname, nil
}

// DetectIP returns a detected IP by running /opt/mesosphere/bin/detect_ip. It will run only once and cache the result.
// When the function is called again, ip will be taken from cache.
func (st *dcosTools) DetectIP() (string, error) {
	if st.ip != "" {
		log.Debugf("Found IP in memory: %s", st.ip)
		return st.ip, nil
	}

	var detectIPCmd string
	// Try to get a path to detect_ip script from environment variable.
	// Variable should be available when start 3dt from systemd. Otherwise hardcode the path.
	detectIPCmd = os.Getenv("MESOS_IP_DISCOVERY_COMMAND")
	if detectIPCmd == "" {
		detectIPCmd = "/opt/mesosphere/bin/detect_ip"
	}
	out, err := exec.Command(detectIPCmd).Output()
	st.ip = strings.TrimRight(string(out), "\n")
	if err != nil {
		return st.ip, err
	}
	log.Debugf("Executed /opt/mesosphere/bin/detect_ip, output: %s", st.ip)
	return st.ip, nil
}

// GetNodeRole returns a nodes role. It will run only once and cache the result.
// When the function is called again, ip will be taken from cache.
func (st *dcosTools) GetNodeRole() (string, error) {
	if st.role != "" {
		return st.role, nil
	}
	if _, err := os.Stat("/etc/mesosphere/roles/master"); err == nil {
		st.role = MasterRole
		return st.role, nil
	}
	if _, err := os.Stat("/etc/mesosphere/roles/slave"); err == nil {
		st.role = AgentRole
		return st.role, nil
	}
	return "", errors.New("Could not determine a role, no /etc/mesosphere/roles/{master,slave} file found")
}

// InitializeDbusConnection opens a dbus connection. The connection is available via st.dcon
func (st *dcosTools) InitializeDbusConnection() (err error) {
	// we need to lock the dbus connection for each request
	st.Lock()
	if st.dcon == nil {
		st.dcon, err = dbus.New()
		if err != nil {
			st.Unlock()
			return err
		}
		return nil
	}
	st.Unlock()
	return errors.New("dbus connection is already opened")
}

// CloseDbusConnection closes a dbus connection.
func (st *dcosTools) CloseDbusConnection() error {
	// unlock the dbus connection no matter what
	defer st.Unlock()
	if st.dcon != nil {
		st.dcon.Close()
		// since dbus api does not provide a way to check that the connection is closed, we'd nil it.
		st.dcon = nil
		return nil
	}
	return errors.New("dbus connection is closed")
}

// GetUnitProperties return a map of systemd unit properties received from dbus.
func (st *dcosTools) GetUnitProperties(pname string) (result map[string]interface{}, err error) {
	// get Service specific properties.
	result, err = st.dcon.GetUnitProperties(pname)
	if err != nil {
		log.Error(err)
		return result, err
	}
	return result, nil
}

// GetUnitNames read a directory /etc/systemd/system/dcos.target.wants and return a list of found systemd units.
func (st *dcosTools) GetUnitNames() (units []string, err error) {
	files, err := ioutil.ReadDir("/etc/systemd/system/dcos.target.wants")
	if err != nil {
		return units, err
	}
	for _, f := range files {
		units = append(units, f.Name())
	}
	log.Debugf("List of units: %s", units)
	return units, nil
}

// GetJournalOutput returns last 50 lines of journald command output for a specific systemd unit.
func (st *dcosTools) GetJournalOutput(unit string) (string, error) {
	out, err := exec.Command("journalctl", "--no-pager", "-n", "50", "-u", unit).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// GetMesosNodeID return a mesos node id.
func (st *dcosTools) GetMesosNodeID(getRole func() (string, error)) (string, error) {
	if st.mesosID != "" {
		log.Debugf("Found in memory mesos node id: %s", st.mesosID)
		return st.mesosID, nil
	}
	role, err := getRole()
	if err != nil {
		return "", err
	}

	roleMesosPort := make(map[string]int)
	roleMesosPort[MasterRole] = 5050
	roleMesosPort[AgentRole] = 5051

	port, ok := roleMesosPort[role]
	if !ok {
		return "", fmt.Errorf("%s role not found", role)
	}
	log.Debugf("using role %s, port %d to get node id", role, port)

	url := fmt.Sprintf("http://%s:%d/state", st.ip, port)

	log.Debugf("GET %s", url)
	resp, err := http.Get(url)
	if err != nil {
		log.Errorf("Could not connect to %s", url)
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	var respJSON map[string]interface{}
	json.Unmarshal(body, &respJSON)
	if id, ok := respJSON["id"]; ok {
		st.mesosID = id.(string)
		log.Debugf("Received node id %s", st.mesosID)
		return st.mesosID, nil
	}
	return "", errors.New("Field id not found")
}

// Help functions
func isInList(item string, l []string) bool {
	for _, listItem := range l {
		if item == listItem {
			return true
		}
	}
	return false
}

func normalizeProperty(unitName string, p map[string]interface{}, d DCOSHelper) healthResponseValues {
	var unitHealth int
	var unitOutput string

	// check keys
	log.Debugf("%s LoadState: %s", unitName, p["LoadState"])
	if p["LoadState"] != "loaded" {
		unitHealth = 1
		unitOutput += fmt.Sprintf("%s is not loaded. Please check `systemctl show all` to check current unit status. ", unitName)
	}

	okStates := []string{"active", "inactive", "activating"}
	log.Debugf("%s ActiveState: %s", unitName, p["ActiveState"])
	if !isInList(p["ActiveState"].(string), okStates) {
		unitHealth = 1
		unitOutput += fmt.Sprintf("%s state is not one of the possible states %s. Current state is [ %s ]. Please check `systemctl show all %s` to check current unit state. ", unitName, okStates, p["ActiveState"], unitName)
	}

	if unitHealth > 0 {
		journalOutput, err := d.GetJournalOutput(unitName)
		if err == nil {
			unitOutput += "\n"
			unitOutput += journalOutput
		} else {
			log.Error(err)
		}
	}

	var prettyName, description string
	s := strings.Split(p["Description"].(string), ": ")
	if len(s) != 2 {
		description = strings.Join(s, " ")

	} else {
		prettyName, description = s[0], s[1]
	}

	return healthResponseValues{
		UnitID:     unitName,
		UnitHealth: unitHealth,
		UnitOutput: unitOutput,
		UnitTitle:  description,
		Help:       "",
		PrettyName: prettyName,
	}
}

func getHostVirtualMemory() (mem.VirtualMemoryStat, error) {
	vmem, err := mem.VirtualMemory()
	if err != nil {
		return *vmem, err
	}
	return *vmem, nil
}

func getHostLoadAvarage() (load.AvgStat, error) {
	la, err := load.Avg()
	if err != nil {
		return *la, err
	}
	return *la, nil
}

func getHostDiskPartitions(all bool) (diskPartitions []disk.PartitionStat, err error) {
	diskPartitions, err = disk.Partitions(all)
	if err != nil {
		return diskPartitions, err
	}
	return diskPartitions, nil
}

func getHostDiskUsage(diskPartitions []disk.PartitionStat) (diskUsage []disk.UsageStat, err error) {
	for _, diskProps := range diskPartitions {
		currentDiskUsage, err := disk.Usage(diskProps.Mountpoint)
		if err != nil {
			// Just log the error, do not return.
			log.Error(err)
			continue
		}
		// Skip the virtual partitions e.g. /proc
		if currentDiskUsage.String() == "" {
			continue
		}
		diskUsage = append(diskUsage, *currentDiskUsage)
	}
	return diskUsage, nil
}

func updateSystemMetrics() (sysMetrics sysMetrics, err error) {
	// Try to update system metrics. Do not return if we could not update some of them.
	if sysMetrics.Memory, err = getHostVirtualMemory(); err != nil {
		log.Error(err)
	}

	if sysMetrics.LoadAvarage, err = getHostLoadAvarage(); err != nil {
		log.Error(err)
	}

	// Get all partitions available on a host.
	if sysMetrics.Partitions, err = getHostDiskPartitions(true); err != nil {
		// If we could not get a list of partitions then return. Disk usage requires a list of partitions.
		return sysMetrics, err
	}

	if sysMetrics.DiskUsage, err = getHostDiskUsage(sysMetrics.Partitions); err != nil {
		return sysMetrics, err
	}

	return sysMetrics, nil
}

func logError(err error) {
	if err != nil {
		log.Error(err)
	}
}

// GetUnitsProperties return a structured units health response of UnitsHealthResponseJsonStruct type.
func GetUnitsProperties(config *Config) (healthReport UnitsHealthResponseJSONStruct, err error) {
	// update system metrics first to make sure we always return them.
	sysMetrics, err := updateSystemMetrics()
	if err != nil {
		log.Error(err)
	}
	healthReport.System = sysMetrics

	// detect DC/OS systemd units
	foundUnits, err := config.DCOSTools.GetUnitNames()
	if err != nil {
		log.Error(err)
	}
	var allUnitsProperties []healthResponseValues
	// open dbus connection
	if err = config.DCOSTools.InitializeDbusConnection(); err != nil {
		return healthReport, err
	}
	log.Debug("Opened dbus connection")

	// DCOS-5862 blacklist systemd units
	excludeUnits := []string{"dcos-setup.service", "dcos-link-env.service", "dcos-download.service"}

	units := append(config.SystemdUnits, foundUnits...)
	for _, unit := range units {
		if isInList(unit, excludeUnits) {
			log.Debugf("Skipping blacklisted systemd unit %s", unit)
			continue
		}
		currentProperty, err := config.DCOSTools.GetUnitProperties(unit)
		if err != nil {
			log.Errorf("Could not get properties for unit: %s", unit)
			continue
		}
		allUnitsProperties = append(allUnitsProperties, normalizeProperty(unit, currentProperty, config.DCOSTools))
	}
	// after we finished querying systemd units, close dbus connection
	if err = config.DCOSTools.CloseDbusConnection(); err != nil {
		// we should probably return here, since we cannot guarantee that all units have been queried.
		return healthReport, err
	}

	// update the rest of healthReport fields
	healthReport.Array = allUnitsProperties
	healthReport.Hostname, err = config.DCOSTools.GetHostname()
	logError(err)

	healthReport.IPAddress, err = config.DCOSTools.DetectIP()
	logError(err)

	healthReport.DcosVersion = config.DCOSVersion
	healthReport.Role, err = config.DCOSTools.GetNodeRole()
	logError(err)

	healthReport.MesosID, err = config.DCOSTools.GetMesosNodeID(config.DCOSTools.GetNodeRole)
	logError(err)
	healthReport.TdtVersion = config.Version

	return healthReport, nil
}