package internal

var (
	Version   = "0.1.0"
	GitCommit = "unknown"
	BuildDate = "unknown"
	GitTag    = "unknown"
)

func FullVersion() string {
	if Version == "dev" && GitCommit != "unknown" {
		return "dev+" + GitCommit[:8]
	}
	return Version
}

func ShortVersion() string {
	return Version
}
