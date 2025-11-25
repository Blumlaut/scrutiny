package detect

import (
	"fmt"
	"os"
	"strconv"
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
	// Run mdadm scan to find all arrays
	args := strings.Split(d.Config.GetString("commands.metrics_mdadm_scan_args"), " ")
	output, err := d.Shell.Command(d.Logger, "mdadm", args, "", os.Environ())
	if err != nil {
		d.Logger.Debugf("Failed to scan mdadm arrays: %v", err)
		return []models.Device{}, nil // Return empty array instead of error
	}
	
	// Parse mdadm output to extract array information
	// The output format is typically like:
	// ARRAY /dev/md/0 level=raid5 num-devices=4 metadata=1.2 UUID=d42fe227:3d36d562:be3be601:118be575
	//   devices=/dev/sda,/dev/sdb,/dev/sdc,/dev/sdd
	
	var devices []models.Device
	
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ARRAY ") {
			// Parse ARRAY line
			device := models.Device{
				DeviceType: "mdadm",
			}
			
			// Extract device name and key-value pairs from the same line
			parts := strings.Split(line, " ")
			if len(parts) > 1 {
				deviceName := strings.TrimSpace(parts[1])
				if strings.HasPrefix(deviceName, "/dev/md/") {
					// For mdadm arrays, we need to store the full path for proper handling later
					// The device name should be the full path like "/dev/md/0"
					device.DeviceName = deviceName
					// Also ensure DeviceType is properly set
					device.DeviceType = "mdadm"
				} else if strings.HasPrefix(deviceName, "/dev/") {
					// For regular devices like /dev/sda, extract just the device name part
					device.DeviceName = strings.TrimPrefix(deviceName, "/dev/")
				}

				// Extract key-value pairs from the line
				// Format: ARRAY /dev/md/0 level=raid5 num-devices=4 metadata=1.2 UUID=d42fe227:3d36d562:be3be601:118be575
				// We need to parse the key=value pairs
				for _, part := range parts[1:] {
					if strings.Contains(part, "=") {
						kv := strings.SplitN(part, "=", 2)
						if len(kv) == 2 {
							key := kv[0]
							value := kv[1]
							switch key {
							case "level":
								device.RaidLevel = value
							case "num-devices":
								if num, err := strconv.Atoi(value); err == nil {
									device.RaidDevices = num
								}
							case "UUID":
								device.WWN = value
							}
						}
					}
				}
			}
			
			// If we have a valid device name, add it to the list
			if len(device.DeviceName) > 0 {
				devices = append(devices, device)
			}
		}
	}
	
	return devices, nil
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
	// Skip SmartCtlInfo for mdadm devices as they don't have SMART data
	for ndx, device := range detectedDevices {
		if device.DeviceType == "mdadm" {
			// For mdadm arrays, we already have the information from DetectMdadmArrays
			// Skip SmartCtlInfo and just populate udev info
			populateUdevInfo(&detectedDevices[ndx]) //ignore errors.
		} else {
			// For regular devices, run SmartCtlInfo to get SMART data
			d.SmartCtlInfo(&detectedDevices[ndx]) //ignore errors.
			populateUdevInfo(&detectedDevices[ndx]) //ignore errors.
		}
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
