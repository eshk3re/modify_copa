package patch

import (
    "bytes"
    "context"
    "errors"
    "fmt"
    "io"
    "os"
    "strings"
    "time"

    "github.com/containerd/platforms"
    "github.com/distribution/reference"
    "github.com/docker/buildx/build"
    "github.com/docker/cli/cli/config"
    log "github.com/sirupsen/logrus"
    "golang.org/x/exp/slices"
    "golang.org/x/sync/errgroup"

    "github.com/moby/buildkit/client"
    "github.com/moby/buildkit/client/llb"
    "github.com/moby/buildkit/exporter/containerimage/exptypes"
    gwclient "github.com/moby/buildkit/frontend/gateway/client"
    "github.com/moby/buildkit/session"
    "github.com/moby/buildkit/session/auth/authprovider"
    "github.com/moby/buildkit/util/progress/progressui"

    "github.com/project-copacetic/copacetic/pkg/buildkit"
    "github.com/project-copacetic/copacetic/pkg/pkgmgr"
    "github.com/project-copacetic/copacetic/pkg/report"
    "github.com/project-copacetic/copacetic/pkg/types/unversioned"
    "github.com/project-copacetic/copacetic/pkg/utils"
    "github.com/project-copacetic/copacetic/pkg/vex"
    "github.com/quay/claircore/osrelease"
)

const (
    defaultPatchedTagSuffix = "patched"
    copaProduct             = "copa"
    defaultRegistry         = "docker.io"
    defaultTag              = "latest"
)


func Patch(
    ctx context.Context,
    timeout time.Duration,
    image, reportFile, patchedTag, workingFolder, scanner, format, output string,
    ignoreError bool,
    bkOpts buildkit.Opts,
    tarOutputPath string,
) error {

    timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()

    ch := make(chan error)
    go func() {
        ch <- patchWithContext(
            timeoutCtx,
            ch,
            image, reportFile, patchedTag, workingFolder, scanner, format, output,
            ignoreError, bkOpts, tarOutputPath,
        )
    }()

    select {
    case err := <-ch:
        return err
    case <-timeoutCtx.Done():
        // add a grace period for long running deferred cleanup functions to complete
        <-time.After(1 * time.Second)
        err := fmt.Errorf("patch exceeded timeout %v", timeout)
        log.Error(err)
        return err
    }
}

func removeIfNotDebug(workingFolder string) {
    if log.GetLevel() >= log.DebugLevel {
        // Keep the intermediate outputs if debugging
        log.Warnf("--debug specified, working folder at %s needs to be manually cleaned up", workingFolder)
    } else {
        os.RemoveAll(workingFolder)
    }
}

func patchWithContext(
    ctx context.Context,
    ch chan error,
    image, reportFile, patchedTag, workingFolder, scanner, format, output string,
    ignoreError bool,
    bkOpts buildkit.Opts,
    tarOutputPath string,
) error {

    imageName, err := reference.ParseNormalizedNamed(image)
    if err != nil {
        return err
    }
    if reference.IsNameOnly(imageName) {
        log.Warnf("Image name has no tag or digest, using latest as tag")
        imageName = reference.TagNameOnly(imageName)
    }

    var tag string
    if taggedName, ok := imageName.(reference.Tagged); ok {
        tag = taggedName.Tag()
    } else {
        log.Warnf("Image name has no tag")
    }

    if patchedTag == "" {
        if tag == "" {
            log.Warnf("No output tag specified for digest-referenced image, defaulting to `%s`", defaultPatchedTagSuffix)
            patchedTag = defaultPatchedTagSuffix
        } else {
            patchedTag = fmt.Sprintf("%s-%s", tag, defaultPatchedTagSuffix)
        }
    }

    _, err = reference.WithTag(imageName, patchedTag)
    if err != nil {
        return fmt.Errorf("%w with patched tag %s", err, patchedTag)
    }
    patchedImageName := fmt.Sprintf("%s:%s", imageName.Name(), patchedTag)

    if workingFolder == "" {
        workingFolder, err = os.MkdirTemp("", "copa-*")
        if err != nil {
            return err
        }
        defer removeIfNotDebug(workingFolder)
        if err = os.Chmod(workingFolder, 0o744); err != nil {
            return err
        }
    } else {
        if isNew, errEns := utils.EnsurePath(workingFolder, 0o744); errEns != nil {
            log.Errorf("failed to create workingFolder %s", workingFolder)
            return errEns
        } else if isNew {
            defer removeIfNotDebug(workingFolder)
        }
    }

    var updates *unversioned.UpdateManifest
    if reportFile != "" {
        updates, err = report.TryParseScanReport(reportFile, scanner)
        if err != nil {
            return err
        }
        log.Debugf("updates to apply: %v", updates)
    }

    bkClient, err := buildkit.NewClient(ctx, bkOpts)
    if err != nil {
        return err
    }
    defer bkClient.Close()

    dockerConfig := config.LoadDefaultConfigFile(os.Stderr)
    attachable := []session.Attachable{
        authprovider.NewDockerAuthProvider(dockerConfig, nil),
    }

    solveOpt := client.SolveOpt{
        Exports: []client.ExportEntry{
            {
                Type: client.ExporterDocker,
                Attrs: map[string]string{
                    "name": patchedImageName,
                },
                Output: func(_ map[string]string) (io.WriteCloser, error) {
                    tarFile, err := os.Create(tarOutputPath)
                    if err != nil {
                        return nil, fmt.Errorf("failed to create tar file: %w", err)
                    }
                    return tarFile, nil
                },
            },
        },
        Frontend: "",
        Session:  attachable,
    }

    solveOpt.SourcePolicy, err = build.ReadSourcePolicy()
    if err != nil {
        return err
    }
    if solveOpt.SourcePolicy != nil {
        switch {
        case strings.Contains(solveOpt.SourcePolicy.Rules[0].Updates.Identifier, "redhat"):
            err = errors.New("RedHat is not supported via source policies due to BusyBox not being in the RHEL repos\n" +
                "Please use a different RPM-based image")
            return err
        case strings.Contains(solveOpt.SourcePolicy.Rules[0].Updates.Identifier, "rockylinux"):
            err = errors.New("RockyLinux is not supported via source policies due to BusyBox not being in the RockyLinux repos\n" +
                "Please use a different RPM-based image")
            return err
		case strings.Contains(solveOpt.SourcePolicy.Rules[0].Updates.Identifier, "alma"):
			err = errors.New("AlmaLinux is not supported via source policies due to BusyBox not being in the AlmaLinux repos\n" +
				"Please use a different RPM-based image")
			return err
        }
    }

    buildChannel := make(chan *client.SolveStatus)
    eg, ctx2 := errgroup.WithContext(ctx)

    eg.Go(func() error {
        _, err := bkClient.Build(ctx2, solveOpt, copaProduct, func(ctx context.Context, c gwclient.Client) (*gwclient.Result, error) {
            config, err := buildkit.InitializeBuildkitConfig(ctx, c, imageName.String())
            if err != nil {
                ch <- err
                return nil, err
            }

            var manager pkgmgr.PackageManager
            if reportFile == "" {
                fileBytes, err := buildkit.ExtractFileFromState(ctx, c, &config.ImageState, "/etc/os-release")
                if err != nil {
                    ch <- err
                    return nil, fmt.Errorf("unable to extract /etc/os-release file from state %w", err)
                }

                osType, err := getOSType(ctx, fileBytes)
                if err != nil {
                    ch <- err
                    return nil, err
                }
                osVersion, err := getOSVersion(ctx, fileBytes)
                if err != nil {
                    ch <- err
                    return nil, err
                }

                manager, err = pkgmgr.GetPackageManager(osType, osVersion, config, workingFolder)
                if err != nil {
                    ch <- err
                    return nil, err
                }
            } else {
                manager, err = pkgmgr.GetPackageManager(updates.Metadata.OS.Type, updates.Metadata.OS.Version, config, workingFolder)
                if err != nil {
                    ch <- err
                    return nil, err
                }
            }


            patchedImageState, errPkgs, err := manager.InstallUpdates(ctx, updates, ignoreError)
            if err != nil {
                ch <- err
                return nil, err
            }


            platform := platforms.Normalize(platforms.DefaultSpec())
            if platform.OS != "linux" {
                platform.OS = "linux"
            }

            def, err := patchedImageState.Marshal(ctx, llb.Platform(platform))
            if err != nil {
                ch <- err
                return nil, fmt.Errorf("unable to marshal platform definition: %w", err)
            }


            res, err := c.Solve(ctx, gwclient.SolveRequest{
                Definition: def.ToPB(),
                Evaluate:   true,
            })
            if err != nil {
                ch <- err
                return nil, err
            }

            res.AddMeta(exptypes.ExporterImageConfigKey, config.ConfigData)

            if reportFile != "" {
                validatedManifest := &unversioned.UpdateManifest{
                    Metadata: unversioned.Metadata{
                        OS: unversioned.OS{
                            Type:    updates.Metadata.OS.Type,
                            Version: updates.Metadata.OS.Version,
                        },
                        Config: unversioned.Config{
                            Arch: updates.Metadata.Config.Arch,
                        },
                    },
                    Updates: []unversioned.UpdatePackage{},
                }

                for _, update := range updates.Updates {
                    if !slices.Contains(errPkgs, update.Name) {
                        validatedManifest.Updates = append(validatedManifest.Updates, update)
                    }
                }

                if output != "" && len(validatedManifest.Updates) > 0 {
                    if err := vex.TryOutputVexDocument(validatedManifest, manager, patchedImageName, format, output); err != nil {
                        ch <- err
                        return nil, err
                    }
                }
            }

            return res, nil
        }, buildChannel)
        return err
    })


    eg.Go(func() error {
        mode := progressui.AutoMode
        if log.GetLevel() >= log.DebugLevel {
            mode = progressui.PlainMode
        }
        display, err := progressui.NewDisplay(os.Stderr, mode)
        if err != nil {
            return err
        }
        _, err = display.UpdateFrom(ctx2, buildChannel)
        return err
    })

    return eg.Wait()
}

func getOSType(ctx context.Context, osreleaseBytes []byte) (string, error) {
    r := bytes.NewReader(osreleaseBytes)
    osData, err := osrelease.Parse(ctx, r)
    if err != nil {
        return "", fmt.Errorf("unable to parse os-release data %w", err)
    }

    osType := strings.ToLower(osData["NAME"])
    switch {
    case strings.Contains(osType, "alpine"):
        return "alpine", nil
    case strings.Contains(osType, "debian"):
        return "debian", nil
    case strings.Contains(osType, "ubuntu"):
        return "ubuntu", nil
    case strings.Contains(osType, "amazon"):
        return "amazon", nil
    case strings.Contains(osType, "centos"):
        return "centos", nil
    case strings.Contains(osType, "mariner"):
        return "cbl-mariner", nil
    case strings.Contains(osType, "azure linux"):
        return "azurelinux", nil
    case strings.Contains(osType, "red hat"):
        return "redhat", nil
    case strings.Contains(osType, "rocky"):
        return "rocky", nil
    case strings.Contains(osType, "oracle"):
        return "oracle", nil
	case strings.Contains(osType, "alma"):
		return "alma", nil
    default:
        log.Error("unsupported osType ", osType)
        return "", errors.ErrUnsupported
    }
}

func getOSVersion(ctx context.Context, osreleaseBytes []byte) (string, error) {
    r := bytes.NewReader(osreleaseBytes)
    osData, err := osrelease.Parse(ctx, r)
    if err != nil {
        return "", fmt.Errorf("unable to parse os-release data %w", err)
    }

    return osData["VERSION_ID"], nil
}
