package mediabrowser

import (
	"net/http"
	"runtime"
)

// systemInfoDTO is the authenticated GET /System/Info body. Each protocol
// receives exactly the fields its own schema declares, with safe Ether
// values. Filesystem paths the reference exposes (ProgramDataPath, CachePath,
// LogPath, TranscodingTempPath, InternalMetadataPath, ItemsByNamePath,
// WebPath, EncoderLocation) are deliberately never sent.
func (i *instance) systemInfoDTO(r *http.Request) map[string]any {
	address := requestAddress(i.info, r)
	port := requestPort(r)
	if port == 0 {
		port = i.listenerPort()
	}
	osName, osDisplay := operatingSystem()
	m := map[string]any{
		"Id": i.info.ID, "ServerName": i.info.ServerName, "Version": i.info.Version,
		"OperatingSystem": osName, "OperatingSystemDisplayName": osDisplay,
		"HasPendingRestart": false, "IsShuttingDown": false,
		"SupportsLibraryMonitor": false, "WebSocketPortNumber": i.listenerPort(),
		"CompletedInstallations": []any{}, "CanSelfRestart": false,
		"CanLaunchWebBrowser": false, "HasUpdateAvailable": false,
	}
	if i.protocol == "jellyfin" {
		m["ProductName"] = i.info.ProductName
		m["StartupWizardCompleted"] = true
		m["LocalAddress"] = address
		m["SystemArchitecture"] = systemArchitecture()
		m["CastReceiverApplications"] = []any{}
		return m
	}
	m["LocalAddress"] = address
	m["LocalAddresses"] = []string{address}
	m["RemoteAddresses"] = []string{address}
	m["WanAddress"] = address
	m["HttpServerPortNumber"] = port
	m["SupportsHttps"] = false
	m["HttpsPortNumber"] = 8920
	m["IsInMaintenanceMode"] = false
	m["SystemUpdateLevel"] = "Release"
	m["CanSelfUpdate"] = false
	m["WakeOnLanInfo"] = []any{}
	m["SupportsWakeServer"] = false
	m["SupportsAutoRunAtStartup"] = false
	m["SupportsLocalPortConfiguration"] = false
	m["HasImageEnhancers"] = false
	m["HardwareAccelerationRequiresPremiere"] = false
	return m
}

// operatingSystem maps the runtime GOOS to the platform names the reference
// servers report: Jellyfin uses adjectival names, Emby its own set.
func operatingSystem() (name, display string) {
	switch runtime.GOOS {
	case "darwin":
		return "OSX", "Unix"
	case "windows":
		return "Windows", "Windows"
	case "linux":
		return "Linux", "Unix"
	case "freebsd":
		return "FreeBSD", "Unix"
	default:
		return runtime.GOOS, runtime.GOOS
	}
}

// systemArchitecture is Jellyfin's SystemArchitecture enum value.
func systemArchitecture() string {
	switch runtime.GOARCH {
	case "amd64":
		return "X64"
	case "arm64":
		return "Arm64"
	case "386":
		return "X86"
	case "arm":
		return "Arm"
	default:
		return runtime.GOARCH
	}
}
