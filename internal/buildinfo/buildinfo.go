// Package buildinfo reports the commit the running binary was built from.
// /healthz serves it, and the proof console uses it to link each trace step
// to the exact source that produced it.
package buildinfo

import "runtime/debug"

// Info identifies one build.
type Info struct {
	Commit   string // full VCS revision, or "unknown" outside a git checkout
	Modified bool   // built from a working tree with uncommitted changes
}

// stamp is the commit set at link time (-X) by builds without a .git to read,
// such as Render's Docker build. Go's own VCS stamp wins when both exist,
// since only it knows whether the tree was dirty.
var stamp string

// Read returns the build info embedded in the running binary.
func Read() Info {
	var settings []debug.BuildSetting
	if bi, ok := debug.ReadBuildInfo(); ok {
		settings = bi.Settings
	}
	return fromSettings(settings, stamp)
}

func fromSettings(settings []debug.BuildSetting, stamp string) Info {
	info := Info{Commit: "unknown"}
	if stamp != "" {
		info.Commit = stamp
	}
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}

// String is the commit, marked "-dirty" when the tree had uncommitted changes,
// so a deployed binary never claims to match a commit it doesn't.
func (i Info) String() string {
	if i.Modified {
		return i.Commit + "-dirty"
	}
	return i.Commit
}
