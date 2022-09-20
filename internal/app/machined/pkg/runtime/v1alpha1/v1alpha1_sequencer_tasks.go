// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package v1alpha1

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/containerd/cgroups"
	cgroupsv2 "github.com/containerd/cgroups/v2"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/siderolabs/go-blockdevice/blockdevice"
	"github.com/siderolabs/go-blockdevice/blockdevice/partition/gpt"
	"github.com/siderolabs/go-blockdevice/blockdevice/util"
	"github.com/siderolabs/go-pointer"
	"github.com/talos-systems/go-cmd/pkg/cmd"
	"github.com/talos-systems/go-cmd/pkg/cmd/proc"
	"github.com/talos-systems/go-kmsg"
	"github.com/talos-systems/go-procfs/procfs"
	"github.com/talos-systems/go-retry/retry"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	installer "github.com/talos-systems/talos/cmd/installer/pkg/install"
	"github.com/talos-systems/talos/internal/app/machined/internal/install"
	"github.com/talos-systems/talos/internal/app/machined/pkg/runtime"
	"github.com/talos-systems/talos/internal/app/machined/pkg/runtime/v1alpha1/bootloader"
	"github.com/talos-systems/talos/internal/app/machined/pkg/runtime/v1alpha1/bootloader/adv"
	"github.com/talos-systems/talos/internal/app/machined/pkg/runtime/v1alpha1/bootloader/grub"
	"github.com/talos-systems/talos/internal/app/machined/pkg/runtime/v1alpha1/platform"
	perrors "github.com/talos-systems/talos/internal/app/machined/pkg/runtime/v1alpha1/platform/errors"
	"github.com/talos-systems/talos/internal/app/machined/pkg/system"
	"github.com/talos-systems/talos/internal/app/machined/pkg/system/events"
	"github.com/talos-systems/talos/internal/app/machined/pkg/system/services"
	"github.com/talos-systems/talos/internal/app/maintenance"
	"github.com/talos-systems/talos/internal/pkg/cri"
	"github.com/talos-systems/talos/internal/pkg/etcd"
	"github.com/talos-systems/talos/internal/pkg/mount"
	"github.com/talos-systems/talos/internal/pkg/partition"
	"github.com/talos-systems/talos/pkg/conditions"
	"github.com/talos-systems/talos/pkg/images"
	krnl "github.com/talos-systems/talos/pkg/kernel"
	"github.com/talos-systems/talos/pkg/kernel/kspp"
	"github.com/talos-systems/talos/pkg/kubernetes"
	machineapi "github.com/talos-systems/talos/pkg/machinery/api/machine"
	"github.com/talos-systems/talos/pkg/machinery/config"
	"github.com/talos-systems/talos/pkg/machinery/config/configloader"
	"github.com/talos-systems/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/talos-systems/talos/pkg/machinery/config/types/v1alpha1/machine"
	"github.com/talos-systems/talos/pkg/machinery/constants"
	"github.com/talos-systems/talos/pkg/machinery/kernel"
	"github.com/talos-systems/talos/pkg/machinery/resources/k8s"
	resourceruntime "github.com/talos-systems/talos/pkg/machinery/resources/runtime"
	"github.com/talos-systems/talos/pkg/version"
)

// SetupLogger represents the SetupLogger task.
func SetupLogger(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		machinedLog, err := r.Logging().ServiceLog("machined").Writer()
		if err != nil {
			return err
		}

		if r.State().Platform().Mode() == runtime.ModeContainer {
			// send all the logs to machinedLog as well, but skip /dev/kmsg logging
			log.SetOutput(io.MultiWriter(log.Writer(), machinedLog))
			log.SetPrefix("[talos] ")

			return nil
		}

		// disable ratelimiting for kmsg, otherwise logs might be not visible.
		// this should be set via kernel arg, but in case it's not set, try to force it.
		if err = krnl.WriteParam(&kernel.Param{
			Key:   "proc.sys.kernel.printk_devkmsg",
			Value: "on\n",
		}); err != nil {
			var serr syscall.Errno

			if !(errors.As(err, &serr) && serr == syscall.EINVAL) { // ignore EINVAL which is returned when kernel arg is set
				log.Printf("failed setting kernel.printk_devkmsg: %s, error ignored", err)
			}
		}

		if err = kmsg.SetupLogger(nil, "[talos]", machinedLog); err != nil {
			return fmt.Errorf("failed to setup logging: %w", err)
		}

		return nil
	}, "setupLogger"
}

// EnforceKSPPRequirements represents the EnforceKSPPRequirements task.
func EnforceKSPPRequirements(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		if err = resourceruntime.NewKernelParamsSetCondition(r.State().V1Alpha2().Resources(), kspp.GetKernelParams()...).Wait(ctx); err != nil {
			return err
		}

		return kspp.EnforceKSPPKernelParameters()
	}, "enforceKSPPRequirements"
}

// SetupSystemDirectory represents the SetupSystemDirectory task.
func SetupSystemDirectory(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		for _, p := range []string{constants.SystemEtcPath, constants.SystemVarPath, constants.StateMountPoint} {
			if err = os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		}

		for _, p := range []string{constants.SystemRunPath} {
			if err = os.MkdirAll(p, 0o751); err != nil {
				return err
			}
		}

		return nil
	}, "setupSystemDirectory"
}

// CreateSystemCgroups represents the CreateSystemCgroups task.
//
//nolint:gocyclo
func CreateSystemCgroups(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		// in container mode cgroups mode depends on cgroups provided by the container runtime
		if r.State().Platform().Mode() != runtime.ModeContainer {
			// assert that cgroupsv2 is being used when running not in container mode,
			// as Talos sets up cgroupsv2 on its own
			if cgroups.Mode() != cgroups.Unified {
				return fmt.Errorf("cgroupsv2 should be used")
			}
		}

		groups := []struct {
			name      string
			resources *cgroupsv2.Resources
		}{
			{
				name: constants.CgroupInit,
				resources: &cgroupsv2.Resources{
					Memory: &cgroupsv2.Memory{
						Min: pointer.To[int64](constants.CgroupInitReservedMemory),
						Low: pointer.To[int64](constants.CgroupInitReservedMemory * 2),
					},
				},
			},
			{
				name: constants.CgroupSystem,
				resources: &cgroupsv2.Resources{
					Memory: &cgroupsv2.Memory{
						Min: pointer.To[int64](constants.CgroupSystemReservedMemory),
						Low: pointer.To[int64](constants.CgroupSystemReservedMemory * 2),
					},
				},
			},
			{
				name:      constants.CgroupSystemRuntime,
				resources: &cgroupsv2.Resources{},
			},
			{
				name: constants.CgroupPodRuntime,
				resources: &cgroupsv2.Resources{
					Memory: &cgroupsv2.Memory{
						Min: pointer.To[int64](constants.CgroupPodRuntimeReservedMemory),
						Low: pointer.To[int64](constants.CgroupPodRuntimeReservedMemory * 2),
					},
				},
			},
			{
				name: constants.CgroupKubelet,
				resources: &cgroupsv2.Resources{
					Memory: &cgroupsv2.Memory{
						Min: pointer.To[int64](constants.CgroupKubeletReservedMemory),
						Low: pointer.To[int64](constants.CgroupKubeletReservedMemory * 2),
					},
				},
			},
		}

		for _, c := range groups {
			if cgroups.Mode() == cgroups.Unified {
				resources := c.resources

				if r.State().Platform().Mode() == runtime.ModeContainer {
					// don't attempt to set resources in container mode, as they might conflict with the parent cgroup tree
					resources = &cgroupsv2.Resources{}
				}

				cg, err := cgroupsv2.NewManager(constants.CgroupMountPath, c.name, resources)
				if err != nil {
					return fmt.Errorf("failed to create cgroup: %w", err)
				}

				if c.name == constants.CgroupInit {
					if err := cg.AddProc(uint64(os.Getpid())); err != nil {
						return fmt.Errorf("failed to move init process to cgroup: %w", err)
					}
				}
			} else {
				cg, err := cgroups.New(cgroups.V1, cgroups.StaticPath(c.name), &specs.LinuxResources{})
				if err != nil {
					return fmt.Errorf("failed to create cgroup: %w", err)
				}

				if c.name == constants.CgroupInit {
					if err := cg.Add(cgroups.Process{
						Pid: os.Getpid(),
					}); err != nil {
						return fmt.Errorf("failed to move init process to cgroup: %w", err)
					}
				}
			}
		}

		return nil
	}, "CreateSystemCgroups"
}

// MountBPFFS represents the MountBPFFS task.
func MountBPFFS(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var mountpoints *mount.Points

		mountpoints, err = mount.BPFMountPoints()
		if err != nil {
			return err
		}

		return mount.Mount(mountpoints)
	}, "mountBPFFS"
}

// MountCgroups represents the MountCgroups task.
func MountCgroups(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var mountpoints *mount.Points

		mountpoints, err = mount.CGroupMountPoints()
		if err != nil {
			return err
		}

		return mount.Mount(mountpoints)
	}, "mountCgroups"
}

// MountPseudoFilesystems represents the MountPseudoFilesystems task.
func MountPseudoFilesystems(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var mountpoints *mount.Points

		mountpoints, err = mount.PseudoSubMountPoints()
		if err != nil {
			return err
		}

		return mount.Mount(mountpoints)
	}, "mountPseudoFilesystems"
}

// SetRLimit represents the SetRLimit task.
func SetRLimit(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		// TODO(andrewrynhard): Should we read limit from /proc/sys/fs/nr_open?
		return unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 1048576, Max: 1048576})
	}, "setRLimit"
}

// DropCapabilities drops some capabilities so that they can't be restored by child processes.
func DropCapabilities(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		prop, err := krnl.ReadParam(&kernel.Param{Key: "proc.sys.kernel.kexec_load_disabled"})
		if v := strings.TrimSpace(string(prop)); err == nil && v != "0" {
			logger.Printf("kernel.kexec_load_disabled is %v, skipping dropping capabilities", v)

			return nil
		}

		// Drop capabilities from the bounding set effectively disabling it for all forked processes,
		// but keep them for PID 1.
		droppedCapabilities := []cap.Value{
			cap.SYS_BOOT,
			cap.SYS_MODULE,
		}

		iab := cap.IABGetProc()

		for _, val := range droppedCapabilities {
			if err := iab.SetVector(cap.Bound, true, val); err != nil {
				return fmt.Errorf("error removing %s from the bounding set: %w", val, err)
			}
		}

		if err := iab.SetProc(); err != nil {
			return fmt.Errorf("error applying caps: %w", err)
		}

		return nil
	}, "dropCapabilities"
}

// See https://www.kernel.org/doc/Documentation/ABI/testing/ima_policy
var rules = []string{
	"dont_measure fsmagic=0x9fa0",     // PROC_SUPER_MAGIC
	"dont_measure fsmagic=0x62656572", // SYSFS_MAGIC
	"dont_measure fsmagic=0x64626720", // DEBUGFS_MAGIC
	"dont_measure fsmagic=0x1021994",  // TMPFS_MAGIC
	"dont_measure fsmagic=0x1cd1",     // DEVPTS_SUPER_MAGIC
	"dont_measure fsmagic=0x42494e4d", // BINFMTFS_MAGIC
	"dont_measure fsmagic=0x73636673", // SECURITYFS_MAGIC
	"dont_measure fsmagic=0xf97cff8c", // SELINUX_MAGIC
	"dont_measure fsmagic=0x43415d53", // SMACK_MAGIC
	"dont_measure fsmagic=0x27e0eb",   // CGROUP_SUPER_MAGIC
	"dont_measure fsmagic=0x63677270", // CGROUP2_SUPER_MAGIC
	"dont_measure fsmagic=0x6e736673", // NSFS_MAGIC
	"dont_measure fsmagic=0xde5e81e4", // EFIVARFS_MAGIC
	"dont_measure fsmagic=0x58465342", // XFS_MAGIC
	"dont_measure fsmagic=0x794c7630", // OVERLAYFS_SUPER_MAGIC
	"measure func=MMAP_CHECK mask=MAY_EXEC",
	"measure func=BPRM_CHECK mask=MAY_EXEC",
	"measure func=FILE_CHECK mask=^MAY_READ euid=0",
	"measure func=FILE_CHECK mask=^MAY_READ uid=0",
	"measure func=MODULE_CHECK",
	"measure func=FIRMWARE_CHECK",
	"measure func=POLICY_CHECK",
}

// WriteIMAPolicy represents the WriteIMAPolicy task.
func WriteIMAPolicy(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		if _, err = os.Stat("/sys/kernel/security/ima/policy"); os.IsNotExist(err) {
			return fmt.Errorf("policy file does not exist: %w", err)
		}

		f, err := os.OpenFile("/sys/kernel/security/ima/policy", os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}

		defer f.Close() //nolint:errcheck

		for _, line := range rules {
			if _, err = f.WriteString(line + "\n"); err != nil {
				return fmt.Errorf("rule %q is invalid", err)
			}
		}

		return nil
	}, "writeIMAPolicy"
}

const osReleaseTemplate = `
NAME="{{ .Name }}"
ID={{ .ID }}
VERSION_ID={{ .Version }}
PRETTY_NAME="{{ .Name }} ({{ .Version }})"
HOME_URL="https://www.talos.dev/"
BUG_REPORT_URL="https://github.com/talos-systems/talos/issues"
`

// OSRelease renders a valid /etc/os-release file and writes it to disk. The
// node's OS Image field is reported by the node from /etc/os-release.
func OSRelease() (err error) {
	if err = createBindMount(filepath.Join(constants.SystemEtcPath, "os-release"), "/etc/os-release"); err != nil {
		return err
	}

	var (
		v    string
		tmpl *template.Template
	)

	switch version.Tag {
	case "none":
		v = version.SHA
	default:
		v = version.Tag
	}

	data := struct {
		Name    string
		ID      string
		Version string
	}{
		Name:    version.Name,
		ID:      strings.ToLower(version.Name),
		Version: v,
	}

	tmpl, err = template.New("").Parse(osReleaseTemplate)
	if err != nil {
		return err
	}

	var buf []byte

	writer := bytes.NewBuffer(buf)

	err = tmpl.Execute(writer, data)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(constants.SystemEtcPath, "os-release"), writer.Bytes(), 0o644)
}

// createBindMount creates a common way to create a writable source file with a
// bind mounted destination. This is most commonly used for well known files
// under /etc that need to be adjusted during startup.
func createBindMount(src, dst string) (err error) {
	var f *os.File

	if f, err = os.OpenFile(src, os.O_WRONLY|os.O_CREATE, 0o644); err != nil {
		return err
	}

	if err = f.Close(); err != nil {
		return err
	}

	if err = unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("failed to create bind mount for %s: %w", dst, err)
	}

	return nil
}

// CreateOSReleaseFile represents the CreateOSReleaseFile task.
func CreateOSReleaseFile(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		// Create /etc/os-release.
		return OSRelease()
	}, "createOSReleaseFile"
}

// LoadConfig represents the LoadConfig task.
func LoadConfig(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		download := func() error {
			var b []byte

			fetchCtx, ctxCancel := context.WithTimeout(ctx, 70*time.Second)
			defer ctxCancel()

			b, e := fetchConfig(fetchCtx, r)
			if errors.Is(e, perrors.ErrNoConfigSource) {
				logger.Println("machine configuration not found; starting maintenance service")

				// nb: we treat maintenance mode as an "activate"
				// event b/c the user is expected to be able to
				// interact with the system at this point.
				platform.FireEvent(
					ctx,
					r.State().Platform(),
					platform.Event{
						Type:    platform.EventTypeActivate,
						Message: "Talos booted into maintenance mode. Ready for user interaction.",
					},
				)

				b, e = receiveConfigViaMaintenanceService(ctx, logger, r)
				if e != nil {
					return fmt.Errorf("failed to receive config via maintenance service: %w", e)
				}
			}

			if e != nil {
				r.Events().Publish(ctx, &machineapi.ConfigLoadErrorEvent{
					Error: e.Error(),
				})

				platform.FireEvent(
					ctx,
					r.State().Platform(),
					platform.Event{
						Type:    platform.EventTypeFailure,
						Message: "Error fetching Talos machine config.",
					},
				)

				return e
			}

			logger.Printf("storing config in memory")

			cfg, e := r.LoadAndValidateConfig(b)
			if e != nil {
				r.Events().Publish(ctx, &machineapi.ConfigLoadErrorEvent{
					Error: e.Error(),
				})

				platform.FireEvent(
					ctx,
					r.State().Platform(),
					platform.Event{
						Type:    platform.EventTypeFailure,
						Message: "Error loading and validating Talos machine config.",
					},
				)

				return e
			}

			platform.FireEvent(
				ctx,
				r.State().Platform(),
				platform.Event{
					Type:    platform.EventTypeConfigLoaded,
					Message: "Talos machine config loaded successfully.",
				},
			)

			return r.SetConfig(cfg)
		}

		cfg, err := configloader.NewFromFile(constants.ConfigPath)
		if err != nil {
			logger.Printf("downloading config")

			return download()
		}

		if !cfg.Persist() {
			logger.Printf("found existing config, but persistence is disabled, downloading config")

			return download()
		}

		logger.Printf("persistence is enabled, using existing config on disk")

		platform.FireEvent(
			ctx,
			r.State().Platform(),
			platform.Event{
				Type:    platform.EventTypeConfigLoaded,
				Message: "Talos machine config loaded successfully.",
			},
		)

		return r.SetConfig(cfg)
	}, "loadConfig"
}

// SaveConfig represents the SaveConfig task.
func SaveConfig(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var b []byte

		b, err = r.Config().Bytes()
		if err != nil {
			return err
		}

		return os.WriteFile(constants.ConfigPath, b, 0o600)
	}, "saveConfig"
}

func fetchConfig(ctx context.Context, r runtime.Runtime) (out []byte, err error) {
	var b []byte

	if b, err = r.State().Platform().Configuration(ctx, r.State().V1Alpha2().Resources()); err != nil {
		return nil, err
	}

	// Detect if config is a gzip archive and unzip it if so
	contentType := http.DetectContentType(b)
	if contentType == "application/x-gzip" {
		var gzipReader *gzip.Reader

		gzipReader, err = gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("error creating gzip reader: %w", err)
		}

		//nolint:errcheck
		defer gzipReader.Close()

		var unzippedData []byte

		unzippedData, err = io.ReadAll(gzipReader)
		if err != nil {
			return nil, fmt.Errorf("error unzipping machine config: %w", err)
		}

		b = unzippedData
	}

	return b, nil
}

func receiveConfigViaMaintenanceService(ctx context.Context, logger *log.Logger, r runtime.Runtime) ([]byte, error) {
	// add "fake" events to signal when Talos enters and leaves maintenance mode
	r.Events().Publish(ctx, &machineapi.TaskEvent{
		Action: machineapi.TaskEvent_START,
		Task:   "runningMaintenance",
	})

	defer r.Events().Publish(ctx, &machineapi.TaskEvent{
		Action: machineapi.TaskEvent_STOP,
		Task:   "runningMaintenance",
	})

	cfgBytes, err := maintenance.Run(ctx, logger, r)
	if err != nil {
		return nil, fmt.Errorf("maintenance service failed: %w", err)
	}

	provider, err := configloader.NewFromBytes(cfgBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create config provider: %w", err)
	}

	warnings, err := provider.Validate(r.State().Platform().Mode())
	for _, w := range warnings {
		logger.Printf("WARNING:\n%s", w)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to validate config: %w", err)
	}

	processedBytes, err := provider.Bytes()
	if err != nil {
		return nil, fmt.Errorf("failed to export validated config: %w", err)
	}

	return processedBytes, nil
}

// ValidateConfig validates the config.
func ValidateConfig(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		warnings, err := r.Config().Validate(r.State().Platform().Mode())
		for _, w := range warnings {
			logger.Printf("WARNING:\n%s", w)
		}

		return err
	}, "validateConfig"
}

// SetUserEnvVars represents the SetUserEnvVars task.
func SetUserEnvVars(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		for key, val := range r.Config().Machine().Env() {
			if err = os.Setenv(key, val); err != nil {
				return fmt.Errorf("failed to set enivronment variable: %w", err)
			}
		}

		return nil
	}, "setUserEnvVars"
}

// StartContainerd represents the task to start containerd.
func StartContainerd(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		svc := &services.Containerd{}

		system.Services(r).LoadAndStart(svc)

		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		return system.WaitForService(system.StateEventUp, svc.ID(r)).Wait(ctx)
	}, "startContainerd"
}

// WriteUdevRules is the task that writes udev rules to a udev rules file.
func WriteUdevRules(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		rules := r.Config().Machine().Udev().Rules()

		var content strings.Builder

		for _, rule := range rules {
			content.WriteString(strings.ReplaceAll(rule, "\n", "\\\n"))
			content.WriteByte('\n')
		}

		if err = os.WriteFile(constants.UdevRulesPath, []byte(content.String()), 0o644); err != nil {
			return fmt.Errorf("failed writing custom udev rules: %w", err)
		}

		return nil
	}, "writeUdevRules"
}

// StartUdevd represents the task to start udevd.
func StartUdevd(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		svc := &services.Udevd{}

		system.Services(r).LoadAndStart(svc)

		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		return system.WaitForService(system.StateEventUp, svc.ID(r)).Wait(ctx)
	}, "startUdevd"
}

// StartAllServices represents the task to start the system services.
func StartAllServices(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		// nb: Treating the beginning of "service starts" as the activate event for a normal
		// non-maintenance mode boot. At this point, we'd expect the user to
		// start interacting with the system for troubleshooting at least.
		platform.FireEvent(
			ctx,
			r.State().Platform(),
			platform.Event{
				Type:    platform.EventTypeActivate,
				Message: "Talos is ready for user interaction.",
			},
		)

		svcs := system.Services(r)

		// load the kubelet service, but don't start it;
		// KubeletServiceController will start it once it's ready.
		svcs.Load(
			&services.Kubelet{},
		)

		serviceList := []system.Service{
			&services.CRI{},
		}

		switch t := r.Config().Machine().Type(); t {
		case machine.TypeInit:
			serviceList = append(serviceList,
				&services.Trustd{},
				&services.Etcd{Bootstrap: true},
			)
		case machine.TypeControlPlane:
			serviceList = append(serviceList,
				&services.Trustd{},
				&services.Etcd{},
			)
		case machine.TypeWorker:
			// nothing
		case machine.TypeUnknown:
			fallthrough
		default:
			panic(fmt.Sprintf("unexpected machine type %v", t))
		}

		svcs.LoadAndStart(serviceList...)

		all := []conditions.Condition{}

		logger.Printf("waiting for %d services", len(svcs.List()))

		for _, svc := range svcs.List() {
			cond := system.WaitForService(system.StateEventUp, svc.AsProto().GetId())
			all = append(all, cond)
		}

		ctx, cancel := context.WithTimeout(ctx, constants.BootTimeout)
		defer cancel()

		aggregateCondition := conditions.WaitForAll(all...)

		errChan := make(chan error)

		go func() {
			errChan <- aggregateCondition.Wait(ctx)
		}()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			logger.Printf("%s", aggregateCondition.String())

			select {
			case err := <-errChan:
				return err
			case <-ticker.C:
			}
		}
	}, "startAllServices"
}

// StopServicesEphemeral represents the StopServicesEphemeral task.
func StopServicesEphemeral(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		// stopping 'cri' service stops everything which depends on it (kubelet, etcd, ...)
		return system.Services(nil).StopWithRevDepenencies(ctx, "cri", "udevd", "trustd")
	}, "stopServicesForUpgrade"
}

// StopAllServices represents the StopAllServices task.
func StopAllServices(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		system.Services(nil).Shutdown(ctx)

		return nil
	}, "stopAllServices"
}

// MountOverlayFilesystems represents the MountOverlayFilesystems task.
func MountOverlayFilesystems(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var mountpoints *mount.Points

		mountpoints, err = mount.OverlayMountPoints()
		if err != nil {
			return err
		}

		return mount.Mount(mountpoints)
	}, "mountOverlayFilesystems"
}

// SetupSharedFilesystems represents the SetupSharedFilesystems task.
func SetupSharedFilesystems(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		targets := []string{"/", "/var/lib/kubelet", "/etc/cni", "/run"}
		for _, t := range targets {
			if err = unix.Mount("", t, "", unix.MS_SHARED|unix.MS_REC, ""); err != nil {
				return err
			}
		}

		return nil
	}, "setupSharedFilesystems"
}

// SetupVarDirectory represents the SetupVarDirectory task.
func SetupVarDirectory(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		for _, p := range []string{"/var/log/audit", "/var/log/containers", "/var/log/pods", "/var/lib/kubelet", "/var/run/lock", constants.SeccompProfilesDirectory} {
			if err = os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		}

		// Handle Kubernetes directories which need different ownership
		for _, p := range []string{constants.KubernetesAuditLogDir} {
			if err = os.MkdirAll(p, 0o700); err != nil {
				return err
			}

			if err = os.Chown(p, constants.KubernetesAPIServerRunUser, constants.KubernetesAPIServerRunGroup); err != nil {
				return fmt.Errorf("failed to chown %s: %w", p, err)
			}
		}

		return nil
	}, "setupVarDirectory"
}

// MountUserDisks represents the MountUserDisks task.
func MountUserDisks(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		if err = partitionAndFormatDisks(logger, r); err != nil {
			return err
		}

		return mountDisks(r)
	}, "mountUserDisks"
}

// TODO(andrewrynhard): We shouldn't pull in the installer command package
// here.
func partitionAndFormatDisks(logger *log.Logger, r runtime.Runtime) error {
	m := &installer.Manifest{
		Devices: map[string]installer.Device{},
		Targets: map[string][]*installer.Target{},
	}

	for _, disk := range r.Config().Machine().Disks() {
		disk := disk

		if err := func() error {
			bd, err := blockdevice.Open(disk.Device())
			if err != nil {
				return err
			}

			//nolint:errcheck
			defer bd.Close()

			var pt *gpt.GPT

			pt, err = bd.PartitionTable()
			if err != nil {
				if !errors.Is(err, blockdevice.ErrMissingPartitionTable) {
					return err
				}
			}

			// Partitions will be created/recreated if either of the following
			//  conditions are true:
			// - a partition table exists AND there are no partitions
			// - a partition table does not exist

			if pt != nil {
				if len(pt.Partitions().Items()) > 0 {
					logger.Printf(("skipping setup of %q, found existing partitions"), disk.Device())

					return nil
				}
			}

			m.Devices[disk.Device()] = installer.Device{
				Device:                 disk.Device(),
				ResetPartitionTable:    true,
				SkipOverlayMountsCheck: true,
			}

			for _, part := range disk.Partitions() {
				extraTarget := &installer.Target{
					Device: disk.Device(),
					FormatOptions: &partition.FormatOptions{
						Size:           part.Size(),
						Force:          true,
						PartitionType:  partition.LinuxFilesystemData,
						FileSystemType: partition.FilesystemTypeXFS,
					},
				}

				m.Targets[disk.Device()] = append(m.Targets[disk.Device()], extraTarget)
			}

			return nil
		}(); err != nil {
			return err
		}
	}

	return m.Execute()
}

func mountDisks(r runtime.Runtime) (err error) {
	mountpoints := mount.NewMountPoints()

	for _, disk := range r.Config().Machine().Disks() {
		for i, part := range disk.Partitions() {
			var partname string

			partname, err = util.PartPath(disk.Device(), i+1)
			if err != nil {
				return err
			}

			if _, err = os.Stat(part.MountPoint()); errors.Is(err, os.ErrNotExist) {
				if err = os.MkdirAll(part.MountPoint(), 0o700); err != nil {
					return err
				}
			}

			mountpoints.Set(partname, mount.NewMountPoint(partname, part.MountPoint(), "xfs", unix.MS_NOATIME, ""))
		}
	}

	return mount.Mount(mountpoints)
}

func unmountDisks(r runtime.Runtime) (err error) {
	mountpoints := mount.NewMountPoints()

	for _, disk := range r.Config().Machine().Disks() {
		for i, part := range disk.Partitions() {
			var partname string

			partname, err = util.PartPath(disk.Device(), i+1)
			if err != nil {
				return err
			}

			mountpoints.Set(partname, mount.NewMountPoint(partname, part.MountPoint(), "xfs", unix.MS_NOATIME, ""))
		}
	}

	return mount.Unmount(mountpoints)
}

// WriteUserFiles represents the WriteUserFiles task.
//
//nolint:gocyclo,cyclop
func WriteUserFiles(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var result *multierror.Error

		files, err := r.Config().Machine().Files()
		if err != nil {
			return fmt.Errorf("error generating extra files: %w", err)
		}

		for _, f := range files {
			content := f.Content()

			switch f.Op() {
			case "create":
				// Allow create at all times.
			case "overwrite":
				if err = existsAndIsFile(f.Path()); err != nil {
					result = multierror.Append(result, err)

					continue
				}
			case "append":
				if err = existsAndIsFile(f.Path()); err != nil {
					result = multierror.Append(result, err)

					continue
				}

				var existingFileContents []byte

				existingFileContents, err = os.ReadFile(f.Path())
				if err != nil {
					result = multierror.Append(result, err)

					continue
				}

				content = string(existingFileContents) + "\n" + f.Content()
			default:
				result = multierror.Append(result, fmt.Errorf("unknown operation for file %q: %q", f.Path(), f.Op()))

				continue
			}

			if filepath.Dir(f.Path()) == constants.ManifestsDirectory {
				if err = os.WriteFile(f.Path(), []byte(content), f.Permissions()); err != nil {
					result = multierror.Append(result, err)

					continue
				}

				if err = os.Chmod(f.Path(), f.Permissions()); err != nil {
					result = multierror.Append(result, err)

					continue
				}

				continue
			}

			// Determine if supplied path is in /var or not.
			// If not, we'll write it to /var anyways and bind mount below
			p := f.Path()
			inVar := true
			parts := strings.Split(
				strings.TrimLeft(f.Path(), "/"),
				string(os.PathSeparator),
			)

			if parts[0] != "var" {
				p = filepath.Join("/var", f.Path())
				inVar = false
			}

			// We do not want to support creating new files anywhere outside of
			// /var. If a valid use case comes up, we can reconsider then.
			if !inVar && f.Op() == "create" {
				return fmt.Errorf("create operation not allowed outside of /var: %q", f.Path())
			}

			if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				result = multierror.Append(result, err)

				continue
			}

			if err = os.WriteFile(p, []byte(content), f.Permissions()); err != nil {
				result = multierror.Append(result, err)

				continue
			}

			if err = os.Chmod(p, f.Permissions()); err != nil {
				result = multierror.Append(result, err)

				continue
			}

			if !inVar {
				if err = unix.Mount(p, f.Path(), "", unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
					result = multierror.Append(result, fmt.Errorf("failed to create bind mount for %s: %w", p, err))
				}
			}
		}

		return result.ErrorOrNil()
	}, "writeUserFiles"
}

//nolint:deadcode,unused
func doesNotExists(p string) (err error) {
	_, err = os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	return fmt.Errorf("file exists")
}

func existsAndIsFile(p string) (err error) {
	var info os.FileInfo

	info, err = os.Stat(p)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}

		return fmt.Errorf("file must exist: %q", p)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("invalid mode: %q", info.Mode().String())
	}

	return nil
}

// UnmountOverlayFilesystems represents the UnmountOverlayFilesystems task.
func UnmountOverlayFilesystems(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var mountpoints *mount.Points

		mountpoints, err = mount.OverlayMountPoints()
		if err != nil {
			return err
		}

		return mount.Unmount(mountpoints)
	}, "unmountOverlayFilesystems"
}

// UnmountUserDisks represents the UnmountUserDisks task.
func UnmountUserDisks(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		return unmountDisks(r)
	}, "unmountUserDisks"
}

// UnmountPodMounts represents the UnmountPodMounts task.
func UnmountPodMounts(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var b []byte

		if b, err = os.ReadFile("/proc/self/mounts"); err != nil {
			return err
		}

		rdr := bytes.NewReader(b)

		scanner := bufio.NewScanner(rdr)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())

			if len(fields) < 2 {
				continue
			}

			mountpoint := fields[1]
			if strings.HasPrefix(mountpoint, constants.EphemeralMountPoint+"/") {
				logger.Printf("unmounting %s\n", mountpoint)

				if err = unix.Unmount(mountpoint, 0); err != nil {
					if errors.Is(err, syscall.EINVAL) {
						log.Printf("ignoring unmount error %s: %v", mountpoint, err)
					} else {
						return fmt.Errorf("error unmounting %s: %w", mountpoint, err)
					}
				}
			}
		}

		return scanner.Err()
	}, "unmountPodMounts"
}

// UnmountSystemDiskBindMounts represents the UnmountSystemDiskBindMounts task.
func UnmountSystemDiskBindMounts(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		devname := r.State().Machine().Disk().BlockDevice.Device().Name()

		f, err := os.Open("/proc/mounts")
		if err != nil {
			return err
		}

		defer f.Close() //nolint:errcheck

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())

			if len(fields) < 2 {
				continue
			}

			device := strings.ReplaceAll(fields[0], "/dev/mapper", "/dev")
			mountpoint := fields[1]

			if strings.HasPrefix(device, devname) && device != devname {
				logger.Printf("unmounting %s\n", mountpoint)

				if err = unix.Unmount(mountpoint, 0); err != nil {
					if errors.Is(err, syscall.EINVAL) {
						log.Printf("ignoring unmount error %s: %v", mountpoint, err)
					} else {
						return fmt.Errorf("error unmounting %s: %w", mountpoint, err)
					}
				}
			}
		}

		return scanner.Err()
	}, "unmountSystemDiskBindMounts"
}

// CordonAndDrainNode represents the task for stop all containerd tasks in the
// k8s.io namespace.
func CordonAndDrainNode(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		// skip not exist error as it means that the node hasn't fully joined yet
		if _, err = os.Stat("/var/lib/kubelet/pki/kubelet-client-current.pem"); err != nil {
			if os.IsNotExist(err) {
				return nil
			}

			return err
		}

		var nodename string

		if nodename, err = r.NodeName(); err != nil {
			return err
		}

		var kubeHelper *kubernetes.Client

		if kubeHelper, err = kubernetes.NewClientFromKubeletKubeconfig(); err != nil {
			return err
		}

		defer kubeHelper.Close() //nolint:errcheck

		return kubeHelper.CordonAndDrain(ctx, nodename)
	}, "cordonAndDrainNode"
}

// UncordonNode represents the task for mark node as scheduling enabled.
//
// This action undoes the CordonAndDrainNode task.
func UncordonNode(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var nodename string

		if nodename, err = r.NodeName(); err != nil {
			return err
		}

		var kubeHelper *kubernetes.Client

		if err = retry.Constant(5*time.Minute, retry.WithUnits(time.Second), retry.WithErrorLogging(true)).RetryWithContext(ctx,
			func(ctx context.Context) error {
				kubeHelper, err = kubernetes.NewClientFromKubeletKubeconfig()

				return retry.ExpectedError(err)
			}); err != nil {
			return err
		}

		defer kubeHelper.Close() //nolint:errcheck

		if err = kubeHelper.WaitUntilReady(ctx, nodename); err != nil {
			return err
		}

		return kubeHelper.Uncordon(ctx, nodename, false)
	}, "uncordonNode"
}

// LeaveEtcd represents the task for removing a control plane node from etcd.
//
//nolint:gocyclo
func LeaveEtcd(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		_, err = os.Stat(filepath.Join(constants.EtcdDataPath, "/member"))
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}

			return err
		}

		etcdID := (&services.Etcd{}).ID(r)

		services := system.Services(r).List()

		shouldLeaveEtcd := false

		for _, service := range services {
			if service.AsProto().Id != etcdID {
				continue
			}

			//nolint:exhaustive
			switch service.GetState() {
			case events.StateRunning:
				fallthrough
			case events.StateStopping:
				fallthrough
			case events.StateFailed:
				shouldLeaveEtcd = true
			}

			break
		}

		if !shouldLeaveEtcd {
			return nil
		}

		client, err := etcd.NewClientFromControlPlaneIPs(ctx, r.State().V1Alpha2().Resources())
		if err != nil {
			return fmt.Errorf("failed to create etcd client: %w", err)
		}

		//nolint:errcheck
		defer client.Close()

		ctx = clientv3.WithRequireLeader(ctx)

		if err = client.LeaveCluster(ctx); err != nil {
			return fmt.Errorf("failed to leave cluster: %w", err)
		}

		return nil
	}, "leaveEtcd"
}

// RemoveAllPods represents the task for stopping and removing all pods.
func RemoveAllPods(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return stopAndRemoveAllPods(cri.StopAndRemove), "removeAllPods"
}

// StopAllPods represents the task for stopping all pods.
func StopAllPods(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return stopAndRemoveAllPods(cri.StopOnly), "stopAllPods"
}

func waitForKubeletLifecycleFinalizers(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
	logger.Printf("waiting for kubelet lifecycle finalizers")

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	lifecycle := resource.NewMetadata(k8s.NamespaceName, k8s.KubeletLifecycleType, k8s.KubeletLifecycleID, resource.VersionUndefined)

	for {
		ok, err := r.State().V1Alpha2().Resources().Teardown(ctx, lifecycle)
		if err != nil {
			return err
		}

		if ok {
			break
		}

		_, err = r.State().V1Alpha2().Resources().WatchFor(ctx, lifecycle, state.WithFinalizerEmpty())
		if err != nil {
			return err
		}
	}

	return r.State().V1Alpha2().Resources().Destroy(ctx, lifecycle)
}

func stopAndRemoveAllPods(stopAction cri.StopAction) runtime.TaskExecutionFunc {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		if err = waitForKubeletLifecycleFinalizers(ctx, logger, r); err != nil {
			logger.Printf("failed waiting for kubelet lifecycle finalizers: %s", err)
		}

		logger.Printf("shutting down kubelet gracefully")

		shutdownCtx, shutdownCtxCancel := context.WithTimeout(ctx, constants.KubeletShutdownGracePeriod*2)
		defer shutdownCtxCancel()

		if err = r.State().Machine().DBus().WaitShutdown(shutdownCtx); err != nil {
			logger.Printf("failed waiting for inhibit shutdown lock: %s", err)
		}

		if err = system.Services(nil).Stop(ctx, "kubelet"); err != nil {
			return err
		}

		client, err := cri.NewClient("unix://"+constants.CRIContainerdAddress, 10*time.Second)
		if err != nil {
			return err
		}

		//nolint:errcheck
		defer client.Close()

		ctx, cancel := context.WithTimeout(ctx, time.Minute*3)
		defer cancel()

		// We remove pods with POD network mode first so that the CNI can perform
		// any cleanup tasks. If we don't do this, we run the risk of killing the
		// CNI, preventing the CRI from cleaning up the pod's netwokring.

		if err = client.StopAndRemovePodSandboxes(ctx, stopAction, runtimeapi.NamespaceMode_POD, runtimeapi.NamespaceMode_CONTAINER); err != nil {
			return err
		}

		// With the POD network mode pods out of the way, we kill the remaining
		// pods.

		return client.StopAndRemovePodSandboxes(ctx, stopAction)
	}
}

// ResetSystemDisk represents the task to reset the system disk.
func ResetSystemDisk(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		var dev *blockdevice.BlockDevice

		dev, err = blockdevice.Open(r.State().Machine().Disk().Device().Name())
		if err != nil {
			return err
		}

		defer dev.Close() //nolint:errcheck

		return dev.Reset()
	}, "resetSystemDisk"
}

// ResetSystemDiskSpec represents the task to reset the system disk by spec.
func ResetSystemDiskSpec(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		in, ok := data.(runtime.ResetOptions)
		if !ok {
			return fmt.Errorf("unexpected runtime data")
		}

		for _, target := range in.GetSystemDiskTargets() {
			if err = target.Format(); err != nil {
				return fmt.Errorf("failed wiping partition %s: %w", target, err)
			}
		}

		logger.Printf("successfully reset system disk by the spec")

		return nil
	}, "resetSystemDiskSpec"
}

// VerifyDiskAvailability represents the task for verifying that the system
// disk is not in use.
func VerifyDiskAvailability(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		devname := r.State().Machine().Disk().BlockDevice.Device().Name()

		// We MUST close this in order to avoid EBUSY.
		if err = r.State().Machine().Close(); err != nil {
			return err
		}

		// TODO(andrewrynhard): This should be more dynamic. If we ever change the
		// partition scheme there is the chance that 2 is not the correct parition to
		// check.
		partname, err := util.PartPath(devname, 2)
		if err != nil {
			return err
		}

		if _, err = os.Stat(partname); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("ephemeral partition not found: %w", err)
		}

		mountsReported := false

		return retry.Constant(3*time.Minute, retry.WithUnits(500*time.Millisecond)).Retry(func() error {
			if err = tryLock(partname); err != nil {
				if err == unix.EBUSY {
					if !mountsReported {
						// if disk is busy, report mounts for debugging purposes but just once
						// otherwise console might be flooded with messages
						dumpMounts(logger)
						mountsReported = true
					}

					return retry.ExpectedError(fmt.Errorf("ephemeral partition in use: %q", partname))
				}

				return fmt.Errorf("failed to verify ephemeral partition not in use: %w", err)
			}

			return nil
		})
	}, "verifyDiskAvailability"
}

func tryLock(path string) error {
	fd, errno := unix.Open(path, unix.O_RDONLY|unix.O_EXCL|unix.O_CLOEXEC, 0)

	//nolint:errcheck
	defer unix.Close(fd)

	return errno
}

func dumpMounts(logger *log.Logger) {
	mounts, err := os.Open("/proc/mounts")
	if err != nil {
		logger.Printf("failed to read mounts: %s", err)

		return
	}

	defer mounts.Close() //nolint:errcheck

	logger.Printf("contents of /proc/mounts:")

	_, _ = io.Copy(log.Writer(), mounts) //nolint:errcheck
}

// Upgrade represents the task for performing an upgrade.
func Upgrade(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		// This should be checked by the gRPC server, but we double check here just
		// to be safe.
		in, ok := data.(*machineapi.UpgradeRequest)
		if !ok {
			return runtime.ErrInvalidSequenceData
		}

		devname := r.State().Machine().Disk().BlockDevice.Device().Name()

		logger.Printf("performing upgrade via %q", in.GetImage())

		// We pull the installer image when we receive an upgrade request. No need
		// to pull it again.
		err = install.RunInstallerContainer(
			devname, r.State().Platform().Name(),
			in.GetImage(),
			r.Config(),
			install.OptionsFromUpgradeRequest(r, in)...,
		)
		if err != nil {
			return err
		}

		logger.Println("upgrade successful")

		return nil
	}, "upgrade"
}

// LabelNodeAsControlPlane represents the LabelNodeAsControlPlane task.
func LabelNodeAsControlPlane(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		h, err := kubernetes.NewTemporaryClientFromPKI(r.Config().Cluster().CA(), r.Config().Cluster().Endpoint())
		if err != nil {
			return err
		}

		defer h.Close() //nolint:errcheck

		var nodename string

		if nodename, err = r.NodeName(); err != nil {
			return err
		}

		err = retry.Constant(constants.NodeReadyTimeout, retry.WithUnits(3*time.Second), retry.WithErrorLogging(true)).RetryWithContext(ctx, func(ctx context.Context) error {
			if err = h.LabelNodeAsControlPlane(ctx, nodename, !r.Config().Cluster().ScheduleOnControlPlanes()); err != nil {
				return retry.ExpectedError(err)
			}

			return nil
		})

		if err != nil {
			return fmt.Errorf("failed to label node as control-plane: %w", err)
		}

		return nil
	}, "labelNodeAsControlPlane"
}

// UpdateBootloader represents the UpdateBootloader task.
func UpdateBootloader(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		meta, err := bootloader.NewMeta()
		if err != nil {
			return err
		}
		//nolint:errcheck
		defer meta.Close()

		if ok := meta.LegacyADV.DeleteTag(adv.Upgrade); ok {
			logger.Println("removing fallback")

			if err = meta.Write(); err != nil {
				return err
			}
		}

		return nil
	}, "updateBootloader"
}

// Reboot represents the Reboot task.
func Reboot(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		rebootCmd := unix.LINUX_REBOOT_CMD_RESTART

		if r.State().Machine().IsKexecPrepared() {
			rebootCmd = unix.LINUX_REBOOT_CMD_KEXEC
		}

		r.Events().Publish(ctx, &machineapi.RestartEvent{
			Cmd: int64(rebootCmd),
		})

		platform.FireEvent(
			ctx,
			r.State().Platform(),
			platform.Event{
				Type:    platform.EventTypeRebooted,
				Message: "Talos rebooted.",
			},
		)

		return runtime.RebootError{Cmd: rebootCmd}
	}, "reboot"
}

// Shutdown represents the Shutdown task.
func Shutdown(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		cmd := unix.LINUX_REBOOT_CMD_POWER_OFF

		if p := procfs.ProcCmdline().Get(constants.KernelParamShutdown).First(); p != nil {
			if *p == "halt" {
				cmd = unix.LINUX_REBOOT_CMD_HALT
			}
		}

		r.Events().Publish(ctx, &machineapi.RestartEvent{
			Cmd: int64(cmd),
		})

		return runtime.RebootError{Cmd: cmd}
	}, "shutdown"
}

// SaveStateEncryptionConfig saves state partition encryption info in the meta partition.
func SaveStateEncryptionConfig(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		config := r.Config()
		if config == nil {
			return nil
		}

		encryption := config.Machine().SystemDiskEncryption().Get(constants.StatePartitionLabel)
		if encryption == nil {
			return nil
		}

		meta, err := bootloader.NewMeta()
		if err != nil {
			return err
		}
		//nolint:errcheck
		defer meta.Close()

		var data []byte

		if data, err = json.Marshal(encryption); err != nil {
			return err
		}

		if !meta.ADV.SetTagBytes(adv.StateEncryptionConfig, data) {
			return fmt.Errorf("failed to save state encryption config in the META partition")
		}

		return meta.Write()
	}, "SaveStateEncryptionConfig"
}

// MountBootPartition mounts the boot partition.
func MountBootPartition(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		return mount.SystemPartitionMount(r, logger, constants.BootPartitionLabel)
	}, "mountBootPartition"
}

// UnmountBootPartition unmounts the boot partition.
func UnmountBootPartition(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		return mount.SystemPartitionUnmount(r, logger, constants.BootPartitionLabel)
	}, "unmountBootPartition"
}

// MountEFIPartition mounts the EFI partition.
func MountEFIPartition(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		return mount.SystemPartitionMount(r, logger, constants.EFIPartitionLabel)
	}, "mountEFIPartition"
}

// UnmountEFIPartition unmounts the EFI partition.
func UnmountEFIPartition(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		return mount.SystemPartitionUnmount(r, logger, constants.EFIPartitionLabel)
	}, "unmountEFIPartition"
}

// MountStatePartition mounts the system partition.
func MountStatePartition(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		meta, err := bootloader.NewMeta()
		if err != nil {
			return err
		}
		//nolint:errcheck
		defer meta.Close()

		flags := mount.SkipIfMounted

		if seq == runtime.SequenceInitialize {
			flags |= mount.SkipIfNoFilesystem
		}

		opts := []mount.Option{mount.WithFlags(flags)}

		var encryption config.Encryption
		// first try reading encryption from the config
		// config always has the priority here
		if r.Config() != nil && r.Config().Machine() != nil {
			encryption = r.Config().Machine().SystemDiskEncryption().Get(constants.StatePartitionLabel)
		}

		// then try reading it from the META partition
		if encryption == nil {
			var encryptionFromMeta *v1alpha1.EncryptionConfig

			data, ok := meta.ADV.ReadTagBytes(adv.StateEncryptionConfig)
			if ok {
				if err = json.Unmarshal(data, &encryptionFromMeta); err != nil {
					return err
				}

				encryption = encryptionFromMeta
			}
		}

		if encryption != nil {
			opts = append(opts, mount.WithEncryptionConfig(encryption))
		}

		return mount.SystemPartitionMount(r, logger, constants.StatePartitionLabel, opts...)
	}, "mountStatePartition"
}

// UnmountStatePartition unmounts the system partition.
func UnmountStatePartition(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		return mount.SystemPartitionUnmount(r, logger, constants.StatePartitionLabel)
	}, "unmountStatePartition"
}

// MountEphemeralPartition mounts the ephemeral partition.
func MountEphemeralPartition(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		return mount.SystemPartitionMount(r, logger, constants.EphemeralPartitionLabel, mount.WithFlags(mount.Resize))
	}, "mountEphemeralPartition"
}

// UnmountEphemeralPartition unmounts the ephemeral partition.
func UnmountEphemeralPartition(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		return mount.SystemPartitionUnmount(r, logger, constants.EphemeralPartitionLabel)
	}, "unmountEphemeralPartition"
}

// Install mounts or installs the system partitions.
func Install(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		switch {
		case !r.State().Machine().Installed():
			installerImage := r.Config().Machine().Install().Image()
			if installerImage == "" {
				installerImage = images.DefaultInstallerImage
			}

			var disk string

			disk, err = r.Config().Machine().Install().Disk()
			if err != nil {
				return err
			}

			err = install.RunInstallerContainer(
				disk,
				r.State().Platform().Name(),
				installerImage,
				r.Config(),
				install.WithForce(true),
				install.WithZero(r.Config().Machine().Install().Zero()),
				install.WithExtraKernelArgs(r.Config().Machine().Install().ExtraKernelArgs()),
			)
			if err != nil {
				platform.FireEvent(
					ctx,
					r.State().Platform(),
					platform.Event{
						Type:    platform.EventTypeFailure,
						Message: "Talos install failed.",
					},
				)

				return err
			}

			platform.FireEvent(
				ctx,
				r.State().Platform(),
				platform.Event{
					Type:    platform.EventTypeInstalled,
					Message: "Talos installed successfully.",
				},
			)

			logger.Println("install successful")

		case r.State().Machine().IsInstallStaged():
			devname := r.State().Machine().Disk().BlockDevice.Device().Name()

			var options install.Options

			if err = json.Unmarshal(r.State().Machine().StagedInstallOptions(), &options); err != nil {
				return fmt.Errorf("error unserializing install options: %w", err)
			}

			logger.Printf("performing staged upgrade via %q", r.State().Machine().StagedInstallImageRef())

			err = install.RunInstallerContainer(
				devname, r.State().Platform().Name(),
				r.State().Machine().StagedInstallImageRef(),
				r.Config(),
				install.WithOptions(options),
			)
			if err != nil {
				platform.FireEvent(
					ctx,
					r.State().Platform(),
					platform.Event{
						Type:    platform.EventTypeFailure,
						Message: "Talos install failed.",
					},
				)

				return err
			}

			// nb: we don't fire an "activate" event after this one
			// b/c we'd only ever get here if Talos was already
			// installed I believe.
			platform.FireEvent(
				ctx,
				r.State().Platform(),
				platform.Event{
					Type:    platform.EventTypeUpgraded,
					Message: "Talos staged upgrade successful.",
				},
			)

			logger.Println("staged upgrade successful")

		default:
			return fmt.Errorf("unsupported configuration for install task")
		}

		return nil
	}, "install"
}

// ActivateLogicalVolumes represents the task for activating logical volumes.
func ActivateLogicalVolumes(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) (err error) {
		if _, err = cmd.Run("/sbin/lvm", "vgchange", "-ay"); err != nil {
			return fmt.Errorf("failed to activate logical volumes: %w", err)
		}

		return nil
	}, "activateLogicalVolumes"
}

// KexecPrepare loads next boot kernel via kexec_file_load.
//
//nolint:gocyclo
func KexecPrepare(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		if req, ok := data.(*machineapi.RebootRequest); ok {
			if req.Mode == machineapi.RebootRequest_POWERCYCLE {
				log.Print("kexec skipped as reboot with power cycle was requested")

				return nil
			}
		}

		if r.Config() == nil {
			return nil
		}

		conf, err := grub.Read(grub.ConfigPath)
		if err != nil {
			return err
		}

		if conf == nil {
			return nil
		}

		defaultEntry, ok := conf.Entries[conf.Default]
		if !ok {
			return nil
		}

		kernelPath := filepath.Join(constants.BootMountPoint, defaultEntry.Linux)
		initrdPath := filepath.Join(constants.BootMountPoint, defaultEntry.Initrd)

		kernel, err := os.Open(kernelPath)
		if err != nil {
			return err
		}

		defer kernel.Close() //nolint:errcheck

		initrd, err := os.Open(initrdPath)
		if err != nil {
			return err
		}

		defer initrd.Close() //nolint:errcheck

		cmdline := strings.TrimSpace(defaultEntry.Cmdline)

		if err = unix.KexecFileLoad(int(kernel.Fd()), int(initrd.Fd()), cmdline, 0); err != nil {
			switch {
			case errors.Is(err, unix.ENOSYS):
				log.Printf("kexec support is disabled in the kernel")

				return nil
			case errors.Is(err, unix.EPERM):
				log.Printf("kexec support is disabled via sysctl")

				return nil
			case errors.Is(err, unix.EBUSY):
				log.Printf("kexec is busy")

				return nil
			default:
				return fmt.Errorf("error loading kernel for kexec: %w", err)
			}
		}

		log.Printf("prepared kexec environment kernel=%q initrd=%q cmdline=%q", kernelPath, initrdPath, cmdline)

		r.State().Machine().KexecPrepared(true)

		return nil
	}, "kexecPrepare"
}

// StartDBus starts the D-Bus mock.
func StartDBus(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		return r.State().Machine().DBus().Start()
	}, "startDBus"
}

// StopDBus stops the D-Bus mock.
func StopDBus(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		if err := r.State().Machine().DBus().Stop(); err != nil {
			logger.Printf("error stopping D-Bus: %s, ignored", err)
		}

		return nil
	}, "stopDBus"
}

// ForceCleanup kills remaining procs and forces partitions unmount.
func ForceCleanup(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
		if err := proc.KillAll(); err != nil {
			logger.Printf("error killing all procs: %s", err)
		}

		if err := mount.UnmountAll(); err != nil {
			logger.Printf("error unmounting: %s", err)
		}

		return nil
	}, "forceCleanup"
}

func pauseOnFailure(callback func(runtime.Sequence, interface{}) (runtime.TaskExecutionFunc, string),
	timeout time.Duration,
) func(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
	return func(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
		f, name := callback(seq, data)

		return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
			err := f(ctx, logger, r)
			if err != nil {
				logger.Printf("%s failed, rebooting in %.0f minutes. You can use talosctl apply-config or talosctl edit mc to fix the issues, error:\n%s", name, timeout.Minutes(), err)

				timer := time.NewTimer(time.Minute * 5)
				defer timer.Stop()

				select {
				case <-timer.C:
				case <-ctx.Done():
				}
			}

			return err
		}, name
	}
}

func taskErrorHandler(handler func(error, *log.Logger) error, task runtime.TaskSetupFunc) runtime.TaskSetupFunc {
	return func(seq runtime.Sequence, data interface{}) (runtime.TaskExecutionFunc, string) {
		f, name := task(seq, data)

		return func(ctx context.Context, logger *log.Logger, r runtime.Runtime) error {
			err := f(ctx, logger, r)
			if err != nil {
				return handler(err, logger)
			}

			return nil
		}, name
	}
}

func phaseListErrorHandler(handler func(error, *log.Logger) error, phases ...runtime.Phase) PhaseList {
	for _, phase := range phases {
		for i, task := range phase.Tasks {
			phase.Tasks[i] = taskErrorHandler(handler, task)
		}
	}

	return phases
}

func logError(err error, logger *log.Logger) error {
	logger.Printf("WARNING: task failed: %s", err)

	return nil
}
