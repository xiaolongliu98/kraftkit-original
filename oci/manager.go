// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2022, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.
package oci

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	regtypes "github.com/docker/docker/api/types/registry"
	regtool "github.com/genuinetools/reg/registry"
	"github.com/genuinetools/reg/repoutils"
	"github.com/gobwas/glob"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"kraftkit.sh/config"
	"kraftkit.sh/internal/set"
	"kraftkit.sh/internal/version"
	"kraftkit.sh/log"
	"kraftkit.sh/oci/handler"
	"kraftkit.sh/oci/simpleauth"
	"kraftkit.sh/pack"
	"kraftkit.sh/packmanager"
	"kraftkit.sh/unikraft"
	"kraftkit.sh/unikraft/component"
	"kraftkit.sh/unikraft/target"
)

type ociManager struct {
	registries []string
	auths      map[string]config.AuthConfig
	handle     func(ctx context.Context) (context.Context, handler.Handler, error)
}

const OCIFormat pack.PackageFormat = "oci"

// NewOCIManager instantiates a new package manager based on OCI archives.
func NewOCIManager(ctx context.Context, opts ...any) (packmanager.PackageManager, error) {
	manager := ociManager{}

	for _, mopt := range opts {
		opt, ok := mopt.(OCIManagerOption)
		if !ok {
			return nil, fmt.Errorf("cannot cast OCI Manager option")
		}

		if err := opt(ctx, &manager); err != nil {
			return nil, err
		}
	}

	if manager.handle == nil {
		return nil, fmt.Errorf("cannot instantiate OCI Manager without handler")
	}

	return &manager, nil
}

// Update implements packmanager.PackageManager
func (manager *ociManager) Update(ctx context.Context) error {
	return nil
}

// Pack implements packmanager.PackageManager
func (manager *ociManager) Pack(ctx context.Context, entity component.Component, opts ...packmanager.PackOption) ([]pack.Package, error) {
	targ, ok := entity.(target.Target)
	if !ok {
		return nil, fmt.Errorf("entity is not Unikraft target")
	}

	pkg, err := NewPackageFromTarget(ctx, targ, opts...)
	if err != nil {
		return nil, err
	}

	return []pack.Package{pkg}, nil
}

// Unpack implements packmanager.PackageManager
func (manager *ociManager) Unpack(ctx context.Context, entity pack.Package, opts ...packmanager.UnpackOption) ([]component.Component, error) {
	return nil, fmt.Errorf("not implemented: oci.manager.Unpack")
}

// registry is a wrapper method for authenticating and listing OCI repositories
// from a provided domain representing a registry.
func (manager *ociManager) registry(ctx context.Context, domain string) (*regtool.Registry, error) {
	var err error
	var auth regtypes.AuthConfig
	insecure := false

	if a, ok := manager.auths[domain]; ok {
		insecure = !a.VerifySSL

		log.G(ctx).
			WithField("registry", domain).
			WithField("insecure", insecure).
			Debug("authenticating")

		auth = regtypes.AuthConfig{
			Username: a.User,
			Password: a.Token,
		}
	} else {
		auth, err = repoutils.GetAuthConfig("", "", domain)
		if err != nil {
			log.G(ctx).WithField("registry", domain).Warn(err)
		}
	}

	reg, err := regtool.New(ctx, auth, regtool.Opt{
		Domain:   domain,
		Debug:    false,
		SkipPing: true,
		Insecure: insecure,
		Headers: map[string]string{
			"User-Agent": version.UserAgent(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("could not initialize registry: %v", err)
	}

	return reg, nil
}

// processV1IndexManifests is an internal utility method which is able to
// iterate over the supplied slice of ocispec.Descriptors which represent a
// Manifest from an Index.  Based on the provided criterium from the query,
// identify the Descriptor that is compatible and instantiate a pack.Package
// structure from it.
func processV1IndexManifests(ctx context.Context, handle handler.Handler, fullref string, query *packmanager.Query, manifests []ocispec.Descriptor) map[string]pack.Package {
	packs := make(map[string]pack.Package)
	var wg sync.WaitGroup
	wg.Add(len(manifests))
	var mu sync.RWMutex

	for _, descriptor := range manifests {
		go func(descriptor ocispec.Descriptor) {
			defer wg.Done()
			if ok, err := IsOCIDescriptorKraftKitCompatible(&descriptor); !ok {
				log.G(ctx).
					WithField("digest", descriptor.Digest.String()).
					Tracef("incompatible index structure: %s", err.Error())
				return
			}

			if query.Platform() != "" && query.Platform() != descriptor.Platform.OS {
				log.G(ctx).
					WithField("digest", descriptor.Digest.String()).
					WithField("want", query.Platform()).
					WithField("got", descriptor.Platform.OS).
					Trace("skipping manifest: platform does not match query")
				return
			}

			if query.Architecture() != "" && query.Architecture() != descriptor.Platform.Architecture {
				log.G(ctx).
					WithField("digest", descriptor.Digest.String()).
					WithField("want", query.Architecture()).
					WithField("got", descriptor.Platform.Architecture).
					Trace("skipping manifest: architecture does not match query")
				return
			}

			if len(query.KConfig()) > 0 {
				// If the list of requested features is greater than the list of
				// available features, there will be no way for the two to match.  We
				// are searching for a subset of query.KConfig() from
				// m.Platform.OSFeatures to match.
				if len(query.KConfig()) > len(descriptor.Platform.OSFeatures) {
					log.G(ctx).
						WithField("digest", descriptor.Digest.String()).
						Trace("skipping descriptor: query contains more features than available")
					return
				}

				available := set.NewStringSet(descriptor.Platform.OSFeatures...)

				// Iterate through the query's requested set of features and skip only
				// if the descriptor does not contain the requested KConfig feature.
				for _, a := range query.KConfig() {
					if !available.Contains(a) {
						log.G(ctx).
							WithField("digest", descriptor.Digest.String()).
							WithField("feature", a).
							Trace("skipping manifest: missing feature")
						return
					}
				}
			}

			// If we have made it this far, the query has been successfully
			// satisfied by this particular manifest and we can generate a package
			// from it.
			pack, err := NewPackageFromOCIManifestDigest(ctx,
				handle,
				fullref,
				query.Auths(),
				descriptor.Digest,
			)
			if err != nil {
				log.G(ctx).
					WithField("digest", descriptor.Digest.String()).
					Tracef("skipping manifest: could not instantiate package from manifest digest: %s", err.Error())
				return
			}

			checksum, err := PlatformChecksum(pack.String(), descriptor.Platform)
			if err != nil {
				log.G(ctx).
					Debugf("could not calculate platform digest for '%s': %s", descriptor.Digest.String(), err)
				return
			}

			mu.Lock()
			packs[checksum] = pack
			mu.Unlock()
		}(descriptor)
	}

	wg.Wait()

	return packs
}

// Catalog implements packmanager.PackageManager
func (manager *ociManager) Catalog(ctx context.Context, qopts ...packmanager.QueryOption) ([]pack.Package, error) {
	query := packmanager.NewQuery(qopts...)

	// Do not perform a search if a query for a specific type is requested and it
	// does not include the application-type.
	if len(query.Types()) > 0 && !slices.Contains(query.Types(), unikraft.ComponentTypeApp) {
		return nil, nil
	}

	var qglob glob.Glob
	var err error
	packs := make(map[string]pack.Package)
	qname := query.Name()

	if strings.ContainsRune(qname, '*') {
		qglob, err = glob.Compile(qname)
		if err != nil {
			return nil, fmt.Errorf("query name is not globable: %w", err)
		}
	}

	qversion := query.Version()
	// Adjust for the version being suffixed in a prototypical OCI reference
	// format.
	ref, refErr := name.ParseReference(qname,
		name.WithDefaultRegistry(""),
	)
	if refErr == nil {
		qname = ref.Context().Name()
		if ref.Identifier() != "latest" && qversion != "" && ref.Identifier() != qversion {
			return nil, fmt.Errorf("cannot determine which version as name contains version and version query paremeter set")
		} else if qversion == "" {
			qversion = ref.Identifier()
		}
	}

	log.G(ctx).WithFields(query.Fields()).Debug("querying oci catalog")

	ctx, handle, err := manager.handle(ctx)
	if err != nil {
		return nil, err
	}

	var auths map[string]config.AuthConfig
	if query.Auths() == nil {
		auths, err = defaultAuths(ctx)
		if err != nil {
			return nil, fmt.Errorf("could not access credentials: %w", err)
		}
	} else {
		auths = query.Auths()
	}

	// If a direct reference can be made, attempt to generate a package from it.
	if query.Update() && refErr == nil {
		authConfig := &authn.AuthConfig{}

		ropts := []remote.Option{
			remote.WithPlatform(v1.Platform{
				OS:           query.Platform(),
				Architecture: query.Architecture(),
				OSFeatures:   query.KConfig(),
			}),
		}

		// Annoyingly convert between regtypes and authn.
		if auth, ok := auths[ref.Context().RegistryStr()]; ok {
			authConfig.Username = auth.User
			authConfig.Password = auth.Token

			if !auth.VerifySSL {
				rt := http.DefaultTransport.(*http.Transport).Clone()
				rt.TLSClientConfig = &tls.Config{
					InsecureSkipVerify: true,
				}
				ropts = append(ropts,
					remote.WithTransport(rt),
				)
			}

			ropts = append(ropts,
				remote.WithAuth(&simpleauth.SimpleAuthenticator{
					Auth: authConfig,
				}),
			)
		}

		v1ImageIndex, err := remote.Index(ref, ropts...)
		if err != nil {
			log.G(ctx).
				Tracef("could not get index: %v", err)
			goto searchRemoteIndexes
		}

		v1IndexManifest, err := v1ImageIndex.IndexManifest()
		if err != nil {
			log.G(ctx).
				WithField("ref", ref).
				Tracef("could not access the index's manifest object: %s", err.Error())
			goto searchRemoteIndexes
		}

		for checksum, pack := range processV1IndexManifests(ctx,
			handle,
			ref.String(),
			query,
			FromGoogleV1DescriptorToOCISpec(v1IndexManifest.Manifests...),
		) {
			packs[checksum] = pack
		}
	}

searchRemoteIndexes:
	if query.Update() {
		for _, domain := range manager.registries {
			log.G(ctx).
				WithField("registry", domain).
				Trace("querying")

			reg, err := manager.registry(ctx, domain)
			if err != nil {
				log.G(ctx).
					WithField("registry", domain).
					Debugf("could not initialize registry: %v", err)
				continue
			}

			authConfig := &authn.AuthConfig{}
			transport := http.DefaultTransport.(*http.Transport).Clone()

			// Annoyingly convert between regtypes and authn.
			if auth, ok := auths[reg.Domain]; ok {
				authConfig.Username = auth.User
				authConfig.Password = auth.Token

				if !auth.VerifySSL {
					transport.TLSClientConfig = &tls.Config{
						InsecureSkipVerify: true,
					}
				}
			}

			if refErr == nil && ref.Context().RegistryStr() != "" && ref.Context().RegistryStr() != domain {
				log.G(ctx).
					WithField("want", domain).
					WithField("got", ref.Context().RegistryStr()).
					Debug("skipping registry")
				continue
			}

			catalog, err := reg.Catalog(ctx, "")
			if err != nil {
				log.G(ctx).
					WithField("registry", domain).
					Debugf("could not query catalog: %v", err)
				continue
			}

			var wg sync.WaitGroup
			wg.Add(len(catalog))
			var mu sync.RWMutex

			for _, fullref := range catalog {
				go func(fullref string) {
					defer wg.Done()

					// Skip direct references from the remote registry
					if query.Update() && refErr == nil && ref.String() == fullref {
						log.G(ctx).
							WithField("ref", fullref).
							Trace("skipping index: does not exist locally")
						return
					}

					if qfullref := fmt.Sprintf("%s:%s", qname, qversion); len(qname) > 0 && fullref != qfullref {
						log.G(ctx).
							WithField("got", fullref).
							WithField("want", qfullref).
							Trace("skipping index: query name does not match")
						return
					}

					ref, err = name.ParseReference(fullref,
						name.WithDefaultRegistry(domain),
					)
					if err != nil {
						log.G(ctx).
							WithField("ref", fullref).
							Tracef("skipping index: could not parse reference: %s", err.Error())
						return
					}

					index, err := remote.Index(ref,
						remote.WithAuth(&simpleauth.SimpleAuthenticator{
							Auth: authConfig,
						}),
						remote.WithTransport(transport),
						remote.WithPlatform(v1.Platform{
							Architecture: query.Architecture(),
							OS:           query.Platform(),
							OSFeatures:   query.KConfig(),
						}),
					)
					if err != nil {
						log.G(ctx).
							WithField("ref", fullref).
							Tracef("skipping index: could not retrieve image: %s", err.Error())
						return
					}

					v1IndexManifest, err := index.IndexManifest()
					if err != nil {
						log.G(ctx).
							WithField("ref", fullref).
							Tracef("could not access the index's manifest object: %s", err.Error())
						return
					}

					v1ManifestPackages := processV1IndexManifests(ctx,
						handle,
						fullref,
						query,
						FromGoogleV1DescriptorToOCISpec(v1IndexManifest.Manifests...),
					)

					mu.Lock()
					for checksum, pack := range v1ManifestPackages {
						packs[checksum] = pack
					}
					mu.Unlock()
				}(fullref)
			}

			wg.Wait()
		}
	}

	// Access local indexes that are available on the host
	indexes, err := handle.ListIndexes(ctx)
	if err != nil {
		return nil, err
	}

	for fullref, index := range indexes {
		if ok, err := IsOCIIndexKraftKitCompatible(index); !ok {
			log.G(ctx).
				WithField("ref", fullref).
				Tracef("skipping index: incompatible index structure: %s", err.Error())
			continue
		}

		if qglob != nil && !qglob.Match(fullref) {
			log.G(ctx).
				WithField("want", qname).
				WithField("got", fullref).
				Trace("skipping index: glob does not match")
			continue
		} else if qglob == nil {
			if len(qversion) > 0 && len(qname) > 0 {
				if fullref != fmt.Sprintf("%s:%s", qname, qversion) {
					log.G(ctx).
						WithField("want", fmt.Sprintf("%s:%s", qname, qversion)).
						WithField("got", fullref).
						Trace("skipping index: name does not match")
					continue
				}
			} else if len(qname) > 0 && fullref != qname {
				log.G(ctx).
					WithField("want", qname).
					WithField("got", fullref).
					Trace("skipping index: name does not match")
				continue
			}
		}

		for checksum, pack := range processV1IndexManifests(ctx,
			handle,
			fullref,
			query,
			index.Manifests,
		) {
			packs[checksum] = pack
		}
	}

	var ret []pack.Package

	for _, pack := range packs {
		ret = append(ret, pack)
	}

	return ret, nil
}

// SetSources implements packmanager.PackageManager
func (manager *ociManager) SetSources(_ context.Context, sources ...string) error {
	manager.registries = sources
	return nil
}

// AddSource implements packmanager.PackageManager
func (manager *ociManager) AddSource(ctx context.Context, source string) error {
	if manager.registries == nil {
		manager.registries = make([]string, 0)
	}

	manager.registries = append(manager.registries, source)

	return nil
}

// Delete implements packmanager.PackageManager.
func (manager *ociManager) Delete(ctx context.Context, qopts ...packmanager.QueryOption) error {
	packs, err := manager.Catalog(ctx, qopts...)
	if err != nil {
		return err
	}

	var errs []error

	for _, pack := range packs {
		if err := pack.Delete(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// RemoveSource implements packmanager.PackageManager
func (manager *ociManager) RemoveSource(ctx context.Context, source string) error {
	for i, needle := range manager.registries {
		if needle == source {
			ret := make([]string, 0)
			ret = append(ret, manager.registries[:i]...)
			manager.registries = append(ret, manager.registries[i+1:]...)
			break
		}
	}

	return nil
}

// IsCompatible implements packmanager.PackageManager
func (manager *ociManager) IsCompatible(ctx context.Context, source string, qopts ...packmanager.QueryOption) (packmanager.PackageManager, bool, error) {
	ctx, handle, err := manager.handle(ctx)
	if err != nil {
		return nil, false, err
	}

	query := packmanager.NewQuery(qopts...)

	// Check if the provided source is a fully qualified OCI reference
	isLocalImage := func(source string) bool {
		// First try without known registries
		if _, err := handle.ResolveIndex(ctx, source); err == nil {
			return true
		}

		// Now try with known registries
		for _, registry := range manager.registries {
			ref, err := name.ParseReference(source,
				name.WithDefaultRegistry(registry),
			)
			if err != nil {
				continue
			}

			if _, err := handle.ResolveIndex(ctx, ref.Context().String()); err == nil {
				return true
			}
		}

		return false
	}

	// Check if the provided source an OCI Distrubtion Spec capable registry
	isRegistry := func(source string) bool {
		if reg, err := manager.registry(ctx, source); err == nil && reg.Ping(ctx) == nil {
			return true
		}

		return false
	}

	// Check if the provided source is OCI registry
	isRemoteImage := func(source string) bool {
		ref, err := name.ParseReference(source,
			name.WithDefaultRegistry(DefaultRegistry),
		)
		if err != nil {
			return false
		}

		// log.G(ctx).WithField("source", source).Debug("checking if source is registry")
		opts := []crane.Option{
			crane.WithContext(ctx),
			crane.WithUserAgent(version.UserAgent()),
			crane.WithPlatform(&v1.Platform{
				OS:           query.Platform(),
				OSFeatures:   query.KConfig(),
				Architecture: query.Architecture(),
			}),
		}

		if auth, ok := config.G[config.KraftKit](ctx).Auth[ref.Context().Registry.RegistryStr()]; ok {
			// We split up the options for authenticating and the option for
			// "verifying ssl" such that a user can simply disable secure connection
			// to a registry if desired.

			if auth.User != "" && auth.Token != "" {
				log.G(ctx).
					WithField("registry", source).
					Debug("authenticating")

				opts = append(opts,
					crane.WithAuth(authn.FromConfig(authn.AuthConfig{
						Username: auth.User,
						Password: auth.Token,
					})),
				)
			}

			if !auth.VerifySSL {
				rt := http.DefaultTransport.(*http.Transport).Clone()
				rt.TLSClientConfig = &tls.Config{
					InsecureSkipVerify: true,
				}
				opts = append(opts,
					crane.Insecure,
					crane.WithTransport(rt),
				)
			}
		}

		raw, err := crane.Config(source, opts...)
		if err == nil && len(raw) > 0 {
			return true
		}

		log.G(ctx).WithField("source", source).Trace(err)

		return false
	}

	checks := []func(string) bool{
		isLocalImage,
	}

	if query.Update() {
		checks = append(checks,
			isRegistry,
			isRemoteImage,
		)
	}

	for _, check := range checks {
		if check(source) {
			return manager, true, nil
		}
	}

	return nil, false, nil
}

// From implements packmanager.PackageManager
func (manager *ociManager) From(pack.PackageFormat) (packmanager.PackageManager, error) {
	return nil, fmt.Errorf("not possible: oci.manager.From")
}

// Format implements packmanager.PackageManager
func (manager *ociManager) Format() pack.PackageFormat {
	return OCIFormat
}
