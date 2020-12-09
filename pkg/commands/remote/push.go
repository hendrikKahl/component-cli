// SPDX-FileCopyrightText: 2020 SAP SE or an SAP affiliate company and Gardener contributors.
//
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	cdv2 "github.com/gardener/component-spec/bindings-go/apis/v2"
	"github.com/gardener/component-spec/bindings-go/codec"
	"github.com/gardener/component-spec/bindings-go/ctf"
	cdoci "github.com/gardener/component-spec/bindings-go/oci"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/gardener/component-cli/ociclient"
	"github.com/gardener/component-cli/ociclient/cache"
	"github.com/gardener/component-cli/ociclient/credentials"
	"github.com/gardener/component-cli/ociclient/credentials/secretserver"
	"github.com/gardener/component-cli/pkg/logger"
	"github.com/gardener/component-cli/pkg/utils"
)

type pushOptions struct {
	// baseUrl is the oci registry where the component is stored.
	baseUrl string
	// componentName is the unique name of the component in the registry.
	componentName string
	// version is the component version in the oci registry.
	version string
	// componentPath is the path to the directory containing the definition.
	componentPath string
	// allowPlainHttp allows the fallback to http if the oci registry does not support https
	allowPlainHttp bool

	// ref is the oci artifact uri reference to the uploaded component descriptor
	ref string
	// cd is the effective component descriptor
	cd *cdv2.ComponentDescriptor
	// cacheDir defines the oci cache directory
	cacheDir string
	// registryConfigPath defines a path to the dockerconfig.json with the oci registry authentication.
	registryConfigPath string
	// ConcourseConfigPath is the path to the local concourse config file.
	ConcourseConfigPath string
}

// NewPushCommand creates a new definition command to push definitions
func NewPushCommand(ctx context.Context) *cobra.Command {
	opts := &pushOptions{}
	cmd := &cobra.Command{
		Use:   "push [path to component descriptor]",
		Args:  cobra.RangeArgs(1, 4),
		Short: "pushes a component archive to an oci repository",
		Long: `
pushes a component archive with the component descriptor and its local blobs to an oci repository.

The command can be called in 2 different ways:

push [path to component descriptor]
- The cli will read all necessary parameters from the component descriptor.

push [baseurl] [componentname] [version] [path to component descriptor]
- The cli will add the baseurl as repository context and validate the name and version.
`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := opts.Complete(args); err != nil {
				fmt.Println(err.Error())
				os.Exit(1)
			}

			if err := opts.run(ctx, logger.Log); err != nil {
				fmt.Println(err.Error())
				os.Exit(1)
			}

			fmt.Printf("Successfully uploaded %s\n", opts.ref)
		},
	}

	opts.AddFlags(cmd.Flags())

	return cmd
}

func (o *pushOptions) run(ctx context.Context, log logr.Logger) error {
	cache, err := cache.NewCache(log, cache.WithBasePath(o.cacheDir))
	if err != nil {
		return err
	}

	archive, err := ctf.ComponentArchiveFromPath(o.componentPath)
	if err != nil {
		return fmt.Errorf("unable to build component archive: %w", err)
	}

	manifest, err := cdoci.NewManifestBuilder(cache, archive).Build(ctx)
	if err != nil {
		return fmt.Errorf("unable to build oci artifact for component acrchive: %w", err)
	}

	ociOpts := []ociclient.Option{
		ociclient.WithCache{Cache: cache},
		ociclient.WithKnownMediaType(cdoci.ComponentDescriptorConfigMimeType),
		ociclient.WithKnownMediaType(cdoci.ComponentDescriptorTarMimeType),
		ociclient.WithKnownMediaType(cdoci.ComponentDescriptorJSONMimeType),
		ociclient.AllowPlainHttp(o.allowPlainHttp),
	}
	if len(o.registryConfigPath) != 0 {
		keyring, err := credentials.CreateOCIRegistryKeyring(nil, []string{o.registryConfigPath})
		if err != nil {
			return fmt.Errorf("unable to create keyring for registry at %q: %w", o.registryConfigPath, err)
		}
		ociOpts = append(ociOpts, ociclient.WithKeyring(keyring))
	} else {
		keyring, err := secretserver.New().
			FromPath(o.ConcourseConfigPath).
			WithMinPrivileges(secretserver.ReadWrite).
			Build()
		if err != nil {
			return fmt.Errorf("unable to get credentils from secret server: %s", err.Error())
		}
		if keyring != nil {
			ociOpts = append(ociOpts, ociclient.WithKeyring(keyring))
		}
	}

	ociClient, err := ociclient.NewClient(log, ociOpts...)
	if err != nil {
		return err
	}

	return ociClient.PushManifest(ctx, o.ref, manifest)
}

func (o *pushOptions) Complete(args []string) error {
	switch len(args) {
	case 1:
		o.componentPath = args[0]
	case 4:
		o.baseUrl = args[0]
		o.componentName = args[1]
		o.version = args[2]
		o.componentPath = args[3]
	}

	var err error
	o.cacheDir, err = utils.CacheDir()
	if err != nil {
		return fmt.Errorf("unable to get oci cache directory: %w", err)
	}

	if err := o.Validate(); err != nil {
		return err
	}

	info, err := os.Stat(o.componentPath)
	if err != nil {
		return fmt.Errorf("unable to get info for %s: %w", o.componentPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf(`%s is not a directory. 
It is expected that the given path points to a diectory that contains the component descriptor as file in "%s" 
`, o.componentPath, ctf.ComponentDescriptorFileName)
	}

	data, err := ioutil.ReadFile(filepath.Join(o.componentPath, ctf.ComponentDescriptorFileName))
	if err != nil {
		return err
	}
	o.cd = &cdv2.ComponentDescriptor{}
	if err := codec.Decode(data, o.cd); err != nil {
		return err
	}

	if len(o.componentName) != 0 {
		if o.cd.Name != o.componentName {
			return fmt.Errorf("name in component descriptor '%s' does not match the given name '%s'", o.cd.Name, o.componentName)
		}
		if o.cd.Version != o.version {
			return fmt.Errorf("version in component descriptor '%s' does not match the given version '%s'", o.cd.Version, o.version)
		}
	}

	if len(o.baseUrl) != 0 {
		o.cd.RepositoryContexts = append(o.cd.RepositoryContexts, cdv2.RepositoryContext{
			Type:    cdv2.OCIRegistryType,
			BaseURL: o.baseUrl,
		})
	}

	repoCtx := o.cd.GetEffectiveRepositoryContext()
	o.ref, err = cdoci.OCIRef(repoCtx, o.cd.Name, o.cd.Version)
	if err != nil {
		return fmt.Errorf("invalid component reference: %w", err)
	}
	return nil
}

// Validate validates push options
func (o *pushOptions) Validate() error {
	if len(o.componentPath) == 0 {
		return errors.New("a path to the component descriptor must be defined")
	}

	if len(o.cacheDir) == 0 {
		return errors.New("a oci cache directory must be defined")
	}

	// todo: validate references exist
	return nil
}

func (o *pushOptions) AddFlags(fs *pflag.FlagSet) {
	fs.BoolVar(&o.allowPlainHttp, "allow-plain-http", false, "allows the fallback to http if the oci registry does not support https")
	fs.StringVar(&o.registryConfigPath, "registry-config", "", "path to the dockerconfig.json with the oci registry authentication information")
	fs.StringVar(&o.ConcourseConfigPath, "cc-config", "", "path to the local concourse config file")
}