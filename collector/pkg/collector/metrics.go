package collector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/analogj/scrutiny/collector/pkg/common/shell"
	"github.com/analogj/scrutiny/collector/pkg/config"
	"github.com/analogj/scrutiny/collector/pkg/detect"
	"github.com/analogj/scrutiny/collector/pkg/errors"
	"github.com/analogj/scrutiny/collector/pkg/models"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
)

type MetricsCollector struct {
	config config.Interface
	BaseCollector
	apiEndpoint *url.URL
	shell       shell.Interface
}

func CreateMetricsCollector(appConfig config.Interface, logger *logrus.Entry, apiEndpoint string) (MetricsCollector, error) {
	apiEndpointUrl, err := url.Parse(apiEndpoint)
	if err != nil {
		return MetricsCollector{}, err
	}

	sc := MetricsCollector{
		config:      appConfig,
		apiEndpoint: apiEndpointUrl,
		BaseCollector: BaseCollector{
			logger: logger,
		},
		shell: shell.Create(),
	}

	return sc, nil
}

func (mc *MetricsCollector) Run() error {
	err := mc.Validate()
	if err != nil {
		return err
	}

	apiEndpoint, _ := url.Parse(mc.apiEndpoint.String())
	apiEndpoint, _ = apiEndpoint.Parse("api/devices/register") //this acts like filepath.Join()

	deviceRespWrapper := new(models.DeviceWrapper)

	deviceDetector := detect.Detect{
		Logger: mc.logger,
		Config: mc.config,
	}
	rawDetectedStorageDevices, err := deviceDetector.Start()
	if err != nil {
		return err
	}

	//filter any device with empty wwn (they are invalid)
	detectedStorageDevices := lo.Filter[models.Device](rawDetectedStorageDevices, func(dev models.Device, _ int) bool {
		return len(dev.WWN) > 0
	})

	mc.logger.Infoln("Sending detected devices to API, for filtering & validation")
	jsonObj, _ := json.Marshal(detectedStorageDevices)
	mc.logger.Debugf("Detected devices: %v", string(jsonObj))
	err = mc.postJson(apiEndpoint.String(), models.DeviceWrapper{
		Data: detectedStorageDevices,
	}, &deviceRespWrapper)
	if err != nil {
		return err
	}

	if !deviceRespWrapper.Success {
		mc.logger.Errorln("An error occurred while retrieving filtered devices")
		mc.logger.Debugln(deviceRespWrapper)
		return errors.ApiServerCommunicationError("An error occurred while retrieving filtered devices")
	} else {
		mc.logger.Debugln(deviceRespWrapper)
		//var wg sync.WaitGroup
		for _, device := range deviceRespWrapper.Data {
			// execute collection in parallel go-routines
			//wg.Add(1)
			//go mc.Collect(&wg, device.WWN, device.DeviceName, device.DeviceType)
			
			// Check if this is an mdadm device
			if strings.HasPrefix(device.DeviceType, "mdadm") || device.IsRaidArray {
				// Handle mdadm device differently
				mc.CollectMdadm(device.WWN, device.DeviceName, device.DeviceType)
			} else {
				// Handle regular device with existing logic
				mc.Collect(device.WWN, device.DeviceName, device.DeviceType)
			}

			if mc.config.GetInt("commands.metrics_smartctl_wait") > 0 {
				time.Sleep(time.Duration(mc.config.GetInt("commands.metrics_smartctl_wait")) * time.Second)
			}
		}

		//mc.logger.Infoln("Main: Waiting for workers to finish")
		//wg.Wait()
		mc.logger.Infoln("Main: Completed")
	}

	return nil
}

func (mc *MetricsCollector) Validate() error {
	mc.logger.Infoln("Verifying required tools")
	_, lookErr := exec.LookPath(mc.config.GetString("commands.metrics_smartctl_bin"))

	if lookErr != nil {
		return errors.DependencyMissingError(fmt.Sprintf("%s binary is missing", mc.config.GetString("commands.metrics_smartctl_bin")))
	}

	return nil
}

// func (mc *MetricsCollector) Collect(wg *sync.WaitGroup, deviceWWN string, deviceName string, deviceType string) {
func (mc *MetricsCollector) Collect(deviceWWN string, deviceName string, deviceType string) {
	//defer wg.Done()
	if len(deviceWWN) == 0 {
		mc.logger.Errorf("no device WWN detected for %s. Skipping collection for this device (no data association possible).\n", deviceName)
		return
	}
	mc.logger.Infof("Collecting smartctl results for %s\n", deviceName)

	fullDeviceName := fmt.Sprintf("%s%s", detect.DevicePrefix(), deviceName)
	args := strings.Split(mc.config.GetCommandMetricsSmartArgs(fullDeviceName), " ")
	//only include the device type if its a non-standard one. In some cases ata drives are detected as scsi in docker, and metadata is lost.
	if len(deviceType) > 0 && deviceType != "scsi" && deviceType != "ata" {
		args = append(args, "--device", deviceType)
	}
	args = append(args, fullDeviceName)

	result, err := mc.shell.Command(mc.logger, mc.config.GetString("commands.metrics_smartctl_bin"), args, "", os.Environ())
	resultBytes := []byte(result)
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			// smartctl command exited with an error, we should still push the data to the API server
			mc.logger.Errorf("smartctl returned an error code (%d) while processing %s\n", exitError.ExitCode(), deviceName)
			mc.LogSmartctlExitCode(exitError.ExitCode())
			mc.Publish(deviceWWN, resultBytes)
		} else {
			mc.logger.Errorf("error while attempting to execute smartctl: %s\n", deviceName)
			mc.logger.Errorf("ERROR MESSAGE: %v", err)
			mc.logger.Errorf("IGNORING RESULT: %v", result)
		}
		return
	} else {
		//successful run, pass the results directly to webapp backend for parsing and processing.
		mc.Publish(deviceWWN, resultBytes)
	}
}

	// CollectMdadm collects metrics for mdadm RAID arrays
func (mc *MetricsCollector) CollectMdadm(deviceWWN string, deviceName string, deviceType string) {
	// Only process if mdadm command is available
	_, err := exec.LookPath("mdadm")
	if err != nil {
		mc.logger.Warnf("mdadm command not found, skipping RAID array monitoring for %s", deviceName)
		// Even if we can't collect mdadm data, we should still publish minimal data to avoid missing device entries
		mc.Publish(deviceWWN, []byte("{}"))
		return
	}
	
	mc.logger.Infof("Collecting mdadm details for RAID array %s\n", deviceName)
	
	// Run mdadm --detail to get comprehensive array status
	// For mdadm arrays, we need to properly construct the device path
	// The deviceName from detection might be just "0" but we need "/dev/md/0"
	var fullDeviceName string
	if strings.HasPrefix(deviceName, "/dev/md/") {
		fullDeviceName = deviceName
		mc.logger.Infof("Using full device path directly: %s", fullDeviceName)
	} else {
		// Check if deviceName is just a number (like "0") and construct proper path
		if _, err := strconv.Atoi(deviceName); err == nil {
			fullDeviceName = fmt.Sprintf("/dev/md/%s", deviceName)
			mc.logger.Infof("Constructed mdadm device path: %s from numeric device name: %s", fullDeviceName, deviceName)
		} else {
			// If it's not a number, use it as-is but with proper prefix
			fullDeviceName = fmt.Sprintf("/dev/%s", deviceName)
			mc.logger.Infof("Constructed device path: %s from device name: %s", fullDeviceName, deviceName)
		}
	}
	
	// Log the final device path that will be used
	mc.logger.Debugf("Final device path for mdadm command: %s", fullDeviceName)
	
	// Use --detail which provides comprehensive RAID status information
	// This gives detailed output including array configuration, status, devices, etc.
	args := []string{"--detail", fullDeviceName}
	
	result, err := mc.shell.Command(mc.logger, "mdadm", args, "", os.Environ())
	if err != nil {
		mc.logger.Errorf("Error collecting mdadm data for %s: %v", deviceName, err)
		// Even if we can't collect mdadm data, we should still publish minimal data to avoid missing device entries
		mc.Publish(deviceWWN, []byte("{}"))
		return
	}
	
	// Parse mdadm output to extract RAID information and populate device fields
	device := models.Device{
		WWN:          deviceWWN,
		DeviceName:   deviceName,
		DeviceType:   deviceType,
		IsRaidArray:  true,
	}
	
	// Parse basic RAID information from mdadm output using a map for cleaner code
	// Define the mapping of mdadm output keys to device fields
	fieldMappings := map[string]*string{
		"Raid Level":   &device.RaidLevel,
		"Array Size":   &device.ArraySize,
		"Layout":       &device.Layout,
		"Chunk Size":   &device.ChunkSize,
		"State":        &device.ArrayStatus,
	}
	
	// Define the mapping of mdadm output keys to integer fields
	intFieldMappings := map[string]*int{
		"Raid Devices":   &device.RaidDevices,
		"Active Devices": &device.ActiveDevices,
		"Failed Devices": &device.FailedDevices,
		"Working Devices": &device.WorkingDevices,
	}
	
	lines := strings.Split(strings.TrimSpace(result), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		for key, fieldPtr := range fieldMappings {
			if strings.HasPrefix(line, key) {
				if parts := strings.Split(line, ":"); len(parts) > 1 {
					*fieldPtr = strings.TrimSpace(parts[1])
				}
				break // Found a match, move to next line
			}
		}
		
		for key, fieldPtr := range intFieldMappings {
			if strings.HasPrefix(line, key) {
				if parts := strings.Split(line, ":"); len(parts) > 1 {
					if value, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
						*fieldPtr = value
					}
				}
				break // Found a match, move to next line
			}
		}
	}
	
	// Create a JSON payload with the parsed RAID data
	// This ensures the data is properly structured for the frontend
	payload, err := json.Marshal(device)
	if err != nil {
		mc.logger.Errorf("Error marshaling device data for %s: %v", deviceName, err)
		// Fallback to raw mdadm output if marshaling fails
		mc.Publish(deviceWWN, []byte(result))
		return
	}
	
	// Publish the structured data
	mc.Publish(deviceWWN, payload)
}

func (mc *MetricsCollector) Publish(deviceWWN string, payload []byte) error {
	mc.logger.Infof("Publishing smartctl results for %s\n", deviceWWN)

	apiEndpoint, _ := url.Parse(mc.apiEndpoint.String())
	apiEndpoint, _ = apiEndpoint.Parse(fmt.Sprintf("api/device/%s/smart", strings.ToLower(deviceWWN)))

	resp, err := httpClient.Post(apiEndpoint.String(), "application/json", bytes.NewBuffer(payload))
	if err != nil {
		mc.logger.Errorf("An error occurred while publishing SMART data for device (%s): %v", deviceWWN, err)
		return err
	}
	defer resp.Body.Close()

	return nil
}
