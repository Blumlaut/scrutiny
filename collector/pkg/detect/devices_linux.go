package detect

import (
	"fmt"
	"os"
	"strings"

	"github.com/analogj/scrutiny/collector/pkg/common/shell"
	"github.com/analogj/scrutiny/collector/pkg/models"
	"github.com/jaypipes/ghw"
	"io/ioutil"
	"path/filepath"
)

func DevicePrefix() string {
	return "/dev/"
}

func (d *Detect) DetectMdadmArrays() ([]models.Device, error) {
	// Run mdadm --detail --scan --export to find all arrays
	args := strings.Split(d.Config.GetString("commands.metrics_mdadm_scan_args"), " ")
	_, err := d.Shell.Command(d.Logger, "mdadm", args, "", os.Environ())
	if err != nil {
		d.Logger.Debugf("Failed to scan mdadm arrays: %v", err)
		return []models.Device{}, nil // Return empty array instead of error
	}
	
	// For now, we'll return a basic device structure based on what smartctl detects
	// In a more complex implementation, we would parse the mdadm output
	// For now we'll just check if mdadm command exists and return a placeholder
	// The actual parsing would be more complex and require proper mdadm output parsing
	
	// Since we're using smartctl to detect devices, we can let it detect mdadm devices
	// by running the scan and parsing the results
	return []models.Device{}, nil
}

func (d *Detect) Start() ([]models.Device, error) {
	d.Shell = shell.Create()
	
	// Get regular devices using existing smartctl scan
	detectedDevices, err := d.SmartctlScan()
	if err != nil {
		return nil, err
	}
	
	// Add mdadm arrays
	mdadmDevices, err := d.DetectMdadmArrays()
	if err != nil {
		d.Logger.Warnf("Failed to detect mdadm arrays: %v", err)
	} else {
		detectedDevices = append(detectedDevices, mdadmDevices...)
	}

	// Inflate device info for detected devices
	for ndx, _ := range detectedDevices {
		d.SmartCtlInfo(&detectedDevices[ndx]) //ignore errors.
		populateUdevInfo(&detectedDevices[ndx]) //ignore errors.
	}

	return detectedDevices, nil
}

//WWN values NVMe and SCSI
func (d *Detect) wwnFallback(detectedDevice *models.Device) {
	block, err := ghw.Block()
	if err == nil {
		for _, disk := range block.Disks {
			if disk.Name == detectedDevice.DeviceName && strings.ToLower(disk.WWN) != "unknown" {
				d.Logger.Debugf("Found matching block device. WWN: %s", disk.WWN)
				detectedDevice.WWN = disk.WWN
				break
			}
		}
	}

	//no WWN found, or could not open Block devices. Either way, fallback to serial number
	if len(detectedDevice.WWN) == 0 {
		d.Logger.Debugf("WWN is empty, falling back to serial number: %s", detectedDevice.SerialNumber)
		detectedDevice.WWN = detectedDevice.SerialNumber
	}

	//wwn must always be lowercase.
	detectedDevice.WWN = strings.ToLower(detectedDevice.WWN)
}

// as discussed in
// - https://github.com/AnalogJ/scrutiny/issues/225
// - https://github.com/jaypipes/ghw/issues/59#issue-361915216
// udev exposes its data in a standardized way under /run/udev/data/....
func populateUdevInfo(detectedDevice *models.Device) error {
	// Get device major:minor numbers
	// `cat /sys/class/block/sda/dev`
	devNo, err := ioutil.ReadFile(filepath.Join("/sys/class/block/", detectedDevice.DeviceName, "dev"))
	if err != nil {
		return err
	}

	// Look up block device in udev runtime database
	// `cat /run/udev/data/b8:0`
	udevID := "b" + strings.TrimSpace(string(devNo))
	udevBytes, err := ioutil.ReadFile(filepath.Join("/run/udev/data/", udevID))
	if err != nil {
		return err
	}

	deviceMountPaths := []string{}
	udevInfo := make(map[string]string)
	for _, udevLine := range strings.Split(string(udevBytes), "\n") {
		if strings.HasPrefix(udevLine, "E:") {
			if s := strings.SplitN(udevLine[2:], "=", 2); len(s) == 2 {
				udevInfo[s[0]] = s[1]
			}
		} else if strings.HasPrefix(udevLine, "S:") {
			deviceMountPaths = append(deviceMountPaths, udevLine[2:])
		}
	}

	//Set additional device information.
	if deviceLabel, exists := udevInfo["ID_FS_LABEL"]; exists {
		detectedDevice.DeviceLabel = deviceLabel
	}
	if deviceUUID, exists := udevInfo["ID_FS_UUID"]; exists {
		detectedDevice.DeviceUUID = deviceUUID
	}
	if deviceSerialID, exists := udevInfo["ID_SERIAL"]; exists {
		detectedDevice.DeviceSerialID = fmt.Sprintf("%s-%s", udevInfo["ID_BUS"], deviceSerialID)
	}


	return nil
}
