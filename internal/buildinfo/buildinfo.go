package buildinfo

import (
	"os"
	"runtime"
	"strings"
)

var (
	Version   = "dev"
	GitSHA    = "unknown"
	BuildTime = "unknown"
	ImageURI  = ""
)

type Info struct {
	Version     string `json:"version"`
	GitSHA      string `json:"git_sha"`
	BuildTime   string `json:"build_time"`
	ImageURI    string `json:"image_uri,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	GoVersion   string `json:"go_version"`
}

func Current() Info {
	imageURI := firstNonEmpty(os.Getenv("APP_IMAGE_URI"), ImageURI)
	imageDigest := os.Getenv("APP_IMAGE_DIGEST")
	if imageDigest == "" {
		imageDigest = digestFromImageURI(imageURI)
	}

	return Info{
		Version:     firstNonEmpty(os.Getenv("APP_VERSION"), Version),
		GitSHA:      firstNonEmpty(os.Getenv("APP_GIT_SHA"), GitSHA),
		BuildTime:   firstNonEmpty(os.Getenv("APP_BUILD_TIME"), BuildTime),
		ImageURI:    imageURI,
		ImageDigest: imageDigest,
		GoVersion:   runtime.Version(),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func digestFromImageURI(imageURI string) string {
	if idx := strings.LastIndex(imageURI, "@sha256:"); idx >= 0 {
		return imageURI[idx+1:]
	}
	return ""
}
