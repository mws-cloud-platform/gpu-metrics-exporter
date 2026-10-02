package gpumetricsexporter

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// nvidiaDriverVersionFile is where the loaded NVIDIA kernel module reports its
// version. It exists only while the module is loaded.
const nvidiaDriverVersionFile = "/proc/driver/nvidia/version"

// driverVersionPattern is a driver version as NVIDIA numbers them: 550.54.15,
// or 390.157 on the old branches. The bounds keep whatever the file holds from
// bloating the payload with a long run of digits and dots.
var driverVersionPattern = regexp.MustCompile(`^[0-9]{1,8}(\.[0-9]{1,8}){1,3}$`)

// readNvidiaDriverVersion reads the version of the loaded NVIDIA kernel module.
// It comes from the module, not from NVML: it is there on a tick whose NVML
// init failed, and it is not something the libnvidia-ml that nvml_library
// measures reports about itself.
func (e *GpuMetricsExporter) readNvidiaDriverVersion() (string, error) {
	data, err := os.ReadFile(e.nvidiaDriverVersionPath)
	if err != nil {
		return "", err
	}
	return parseNvidiaDriverVersion(string(data))
}

// parseNvidiaDriverVersion takes the version from the NVRM line, which reads
//
//	NVRM version: NVIDIA UNIX x86_64 Kernel Module  535.104.05  Sat Aug 19 01:15:15 UTC 2023
//
// for the proprietary kernel module and
//
//	NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  550.54.15  Release Build  (dvs-builder@U16-I3-B03-4-3)  Tue Mar  5 22:16:25 UTC 2024
//
// for the open one: the first field shaped like a version, which neither the
// architecture before it nor the build date after it is.
func parseNvidiaDriverVersion(content string) (string, error) {
	for _, line := range strings.Split(content, "\n") {
		rest, ok := strings.CutPrefix(line, "NVRM version:")
		if !ok {
			continue
		}
		for _, field := range strings.Fields(rest) {
			if driverVersionPattern.MatchString(field) {
				return field, nil
			}
		}
		return "", fmt.Errorf("no version in %q", truncate(line))
	}
	return "", errors.New("no NVRM version line")
}
