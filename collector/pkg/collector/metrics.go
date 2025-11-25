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

	// Log the final device path that will be used
	mc.logger.Debugf("Final device path for mdadm command: %s", deviceName)
	
	// Use --detail which provides comprehensive RAID status information
	// This gives detailed output including array configuration, status, devices, etc.
	args := []string{"--detail", deviceName}
	
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
	
	// Parse basic RAID information from mdadm output using a more robust approach
	lines := strings.Split(strings.TrimSpace(result), "\n")
	
	// Process each line to extract RAID information
	for _, line := range lines {
		line = strings.TrimSpace(line)
		
		// Skip empty lines and header lines
		if line == "" || strings.HasPrefix(line, "/dev/md") || strings.HasPrefix(line, "Version") {
			continue
		}
		
		// Handle key-value pairs with different formats
		if strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				
				// Clean up the value by removing any leading/trailing whitespace and colon
				if strings.HasPrefix(value, ": ") {
					value = strings.TrimSpace(strings.TrimPrefix(value, ": "))
				} else if strings.HasPrefix(value, ":") {
					value = strings.TrimSpace(strings.TrimPrefix(value, ":"))
				}
				
				switch key {
				case "Raid Level":
					device.RaidLevel = value
				case "Array Size":
					device.ArraySize = value
				case "Layout":
					device.Layout = value
				case "Chunk Size":
					device.ChunkSize = value
				case "State":
					device.ArrayStatus = value
				case "Raid Devices":
					if valueInt, err := strconv.Atoi(value); err == nil {
						device.RaidDevices = valueInt
					}
				case "Active Devices":
					if valueInt, err := strconv.Atoi(value); err == nil {
						device.ActiveDevices = valueInt
					}
				case "Failed Devices":
					if valueInt, err := strconv.Atoi(value); err == nil {
						device.FailedDevices = valueInt
					}
				case "Working Devices":
					if valueInt, err := strconv.Atoi(value); err == nil {
						device.WorkingDevices = valueInt
					}
				case "Device State":
					device.ArrayStatus = value
				case "Array State":
					device.ArrayStatus = value
				}
			}
		} else if strings.Contains(line, " ") && !strings.HasPrefix(line, "Number") {
			// Handle lines with space separators (like "Layout : left-symmetric")
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				// The key is everything before the first space
				key := strings.TrimSuffix(parts[0], ":")
				value := strings.Join(parts[1:], " ")
				
				// Clean up the value
				if strings.HasPrefix(value, ": ") {
					value = strings.TrimSpace(strings.TrimPrefix(value, ": "))
				} else if strings.HasPrefix(value, ":") {
					value = strings.TrimSpace(strings.TrimPrefix(value, ":"))
				}
				
				switch key {
				case "Raid Level":
					device.RaidLevel = value
				case "Array Size":
					device.ArraySize = value
				case "Layout":
					device.Layout = value
				case "Chunk Size":
					device.ChunkSize = value
				case "State":
					device.ArrayStatus = value
				case "Raid Devices":
					if valueInt, err := strconv.Atoi(value); err == nil {
						device.RaidDevices = valueInt
					}
				case "Active Devices":
					if valueInt, err := strconv.Atoi(value); err == nil {
						device.ActiveDevices = valueInt
					}
				case "Failed Devices":
					if valueInt, err := strconv.Atoi(value); err == nil {
						device.FailedDevices = valueInt
					}
				case "Working Devices":
					if valueInt, err := strconv.Atoi(value); err == nil {
						device.WorkingDevices = valueInt
					}
				case "Device State":
					device.ArrayStatus = value
				}
			}
		}
	}
	
	// Create a JSON payload with the parsed RAID data
	// This ensures the data is properly structured for the frontend
	payload, err := json.Marshal(device)
	mc.logger.Infof("data: %s", payload)
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
