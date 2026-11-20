package inject

import (
	"path"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// join is one container variable that points at a file the interceptor
// extends with the pod CA.
type join struct {
	container string
	variable  string
	// source is the file's path inside the interceptor.
	source string
	// mount is the container's own volume mount, read-only, in the interceptor.
	mount corev1.VolumeMount
}

// trustPath is where the interceptor writes the joined file.
func (j join) trustPath() string {
	return trustDir + "/join/" + j.container + "/" + j.variable
}

func joinFor(joins []join, container, variable string) *join {
	for index := range joins {
		if joins[index].container == container && joins[index].variable == variable {
			return &joins[index]
		}
	}

	return nil
}

func unreadable(container, variable string) string {
	return "container \"" + container + "\" sets " + variable + " to a file githubgate can't read; put the CA in a ConfigMap and name it in the namespace annotation " + ExtraCAConfigMapAnnotation
}

// planJoins decides, for every container that sets one of the CA
// variables, how the interceptor reads that variable's file. It returns a
// rejection reason when a file can't be read. prior is the interceptor from
// an earlier run, whose joins are kept.
func planJoins(pod *corev1.Pod, prior *corev1.Container) ([]join, string) {
	var joins []join

	for _, group := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for index := range group {
			container := &group[index]
			if container.Name == SidecarName {
				continue
			}

			for _, variable := range trustVars {
				env := lastEnv(container, variable.name)
				if env == nil {
					continue
				}

				planned, ok := planJoin(pod, container, variable.name, env, prior, joins)
				if !ok {
					return nil, unreadable(container.Name, variable.name)
				}

				joins = append(joins, planned)
			}
		}
	}

	return joins, ""
}

// lastEnv returns the entry that wins when a variable is set more than once.
func lastEnv(container *corev1.Container, name string) *corev1.EnvVar {
	for index := len(container.Env) - 1; index >= 0; index-- {
		if container.Env[index].Name == name {
			return &container.Env[index]
		}
	}

	return nil
}

func planJoin(pod *corev1.Pod, container *corev1.Container, variable string, env *corev1.EnvVar, prior *corev1.Container, taken []join) (join, bool) {
	planned := join{container: container.Name, variable: variable}

	if env.ValueFrom != nil {
		return planned, false
	}

	if prior != nil && env.Value == planned.trustPath() {
		return kept(prior, planned)
	}

	source := path.Clean(env.Value)
	if !path.IsAbs(source) {
		return planned, false
	}

	var best *corev1.VolumeMount

	for index := range container.VolumeMounts {
		mount := &container.VolumeMounts[index]
		root := path.Clean(mount.MountPath)

		if source != root && !strings.HasPrefix(source, strings.TrimSuffix(root, "/")+"/") {
			continue
		}

		if best == nil || len(path.Clean(best.MountPath)) <= len(root) {
			best = mount
		}
	}

	if best == nil || best.SubPathExpr != "" || !readableVolume(pod, best.Name) {
		return planned, false
	}

	rest := strings.TrimPrefix(source, path.Clean(best.MountPath))
	root := joinRoot + "/" + container.Name + "/" + nextIndex(taken, prior, container.Name)

	planned.source = root + rest
	planned.mount = corev1.VolumeMount{Name: best.Name, MountPath: root, SubPath: best.SubPath, ReadOnly: true}

	return planned, true
}

// kept finds the join the earlier run made for the same variable.
func kept(prior *corev1.Container, planned join) (join, bool) {
	prefix := planned.container + "/" + planned.variable + "="

	for index := 0; index+1 < len(prior.Args); index++ {
		source, found := strings.CutPrefix(prior.Args[index+1], prefix)
		if prior.Args[index] != "--join" || !found {
			continue
		}

		for _, mount := range prior.VolumeMounts {
			if source == mount.MountPath || strings.HasPrefix(source, mount.MountPath+"/") {
				planned.source = source
				planned.mount = mount

				return planned, true
			}
		}
	}

	return planned, false
}

// nextIndex returns the smallest number whose mount path is unused in the container's join directory.
func nextIndex(taken []join, prior *corev1.Container, container string) string {
	used := map[string]bool{}
	for _, joined := range taken {
		used[joined.mount.MountPath] = true
	}

	if prior != nil {
		for _, mount := range prior.VolumeMounts {
			used[mount.MountPath] = true
		}
	}

	for index := 0; ; index++ {
		candidate := strconv.Itoa(index)
		if !used[joinRoot+"/"+container+"/"+candidate] {
			return candidate
		}
	}
}

// readableVolume reports whether the volume is one the interceptor can mount
// the same way: ConfigMap, Secret, projected or emptyDir.
func readableVolume(pod *corev1.Pod, name string) bool {
	index := slices.IndexFunc(pod.Spec.Volumes, func(volume corev1.Volume) bool { return volume.Name == name })
	if index < 0 {
		return false
	}

	source := pod.Spec.Volumes[index].VolumeSource

	return source.ConfigMap != nil || source.Secret != nil || source.Projected != nil || source.EmptyDir != nil
}
