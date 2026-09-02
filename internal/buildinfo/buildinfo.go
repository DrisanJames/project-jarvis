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
	// EnvManifestSHA is the sha256 of deploy/env.manifest.json that rendered
	// this task definition (REQ-092). git_sha answers "which code"; this
	// answers "which config" — the two move independently, and 10 of the last
	// ~90 revisions changed only the config.
	EnvManifestSHA string `json:"env_manifest_sha,omitempty"`
	// TreeDirty is "1" when the image was built from a working tree with
	// uncommitted changes (DEPLOY_ALLOW_DIRTY=1). Without it, two different
	// trees ship under one git_sha and both "verify".
	TreeDirty string `json:"tree_dirty,omitempty"`
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

		EnvManifestSHA: os.Getenv("APP_ENV_MANIFEST_SHA"),
		TreeDirty:      os.Getenv("APP_TREE_DIRTY"),
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
