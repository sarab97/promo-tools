/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	// nolint[lll]
	reg "github.com/kubernetes-sigs/k8s-container-image-promoter/lib/dockerregistry"
	"github.com/kubernetes-sigs/k8s-container-image-promoter/lib/stream"
)

// GitDescribe is stamped by bazel.
var GitDescribe string

// GitCommit is stamped by bazel.
var GitCommit string

// TimestampUtcRfc3339 is stamped by bazel.
var TimestampUtcRfc3339 string

// nolint[gocyclo]
func main() {
	manifestPtr := flag.String(
		"manifest", "", "the manifest file to load (REQUIRED)")
	garbageCollectPtr := flag.Bool(
		"garbage-collect",
		false, "delete all untagged images in the destination registry")
	threadsPtr := flag.Int(
		"threads",
		10, "number of concurrent goroutines to use when talking to GCR")
	verbosityPtr := flag.Int(
		"verbosity",
		2,
		"verbosity level for logging;"+
			" 0 = fatal only,"+
			" 1 = fatal + errors,"+
			" 2 = fatal + errors + warnings,"+
			" 3 = fatal + errors + warnings + informational (everything)")
	deleteExtraTags := flag.Bool(
		"delete-extra-tags",
		false,
		"delete tags in the destination registry that are not declared"+
			" in the Manifest (default: false)")
	parseOnlyPtr := flag.Bool(
		"parse-only",
		false,
		"only check that the given manifest file is parseable as a Manifest"+
			" (default: false)")
	dryRunPtr := flag.Bool(
		"dry-run",
		true,
		"print what would have happened by running this tool;"+
			" do not actually modify any registry")
	// Add in help flag information, because Go's "flag" package automatically
	// adds it, but for whatever reason does not show it as part of available
	// options.
	helpPtr := flag.Bool(
		"help",
		false,
		"print help")
	versionPtr := flag.Bool(
		"version",
		false,
		"print version")
	noSvcAcc := false
	flag.BoolVar(&noSvcAcc, "no-service-account", false,
		"do not pass '--account=...' to all gcloud calls (default: false)")
	flag.Parse()

	if len(os.Args) == 1 {
		printVersion()
		printUsage()
		os.Exit(0)
	}

	if *helpPtr {
		printUsage()
		os.Exit(0)
	}

	if *versionPtr {
		printVersion()
		os.Exit(0)
	}

	if *manifestPtr == "" {
		log.Fatal(fmt.Errorf("-manifest=... flag is required"))
	}
	mfest, err := reg.ParseManifestFromFile(*manifestPtr)
	if err != nil {
		log.Fatal(err)
	}
	if *parseOnlyPtr {
		os.Exit(0)
	}
	// If there are no images in the manifest, it may be a stub manifest file
	// (such as for brand new registries that would be watched by the promoter
	// for the very first time). In any case, we do NOT want to process such
	// manifests, because other logic like garbage collection would think that
	// the manifest desires a completely blank registry. In practice this would
	// almost never be the case, so given a fully-parsed manifest with 0 images,
	// treat it as if -parse-only was implied and exit gracefully.
	if len(mfest.Images) == 0 {
		fmt.Println("No images in manifest --- nothing to do.")
		os.Exit(0)
	}

	if *dryRunPtr {
		fmt.Printf("********** START (DRY RUN): %s **********\n", *manifestPtr)
	} else {
		fmt.Printf("********** START: %s **********\n", *manifestPtr)
	}

	mi := map[reg.RegistryName]reg.RegInvImage{}
	for _, registry := range mfest.Registries {
		mi[registry.Name] = nil
	}
	sc, err := reg.MakeSyncContext(
		*manifestPtr,
		mi,
		*verbosityPtr,
		*threadsPtr,
		*deleteExtraTags,
		*dryRunPtr,
		!noSvcAcc,
		mfest.Registries)
	if err != nil {
		log.Fatal(err)
	}

	// Read the state of the world; i.e., populate the SyncContext.
	mkRegistryListingCmd := func(
		rc reg.RegistryContext) stream.Producer {
		var sp stream.Subprocess
		sp.CmdInvocation = reg.GetRegistryListingCmd(
			rc,
			sc.UseServiceAccount)
		return &sp
	}

	sc.ReadImageNames(mkRegistryListingCmd)
	mkRegistryListTagsCmd := func(
		rc reg.RegistryContext, imgName reg.ImageName) stream.Producer {
		var sp stream.Subprocess
		sp.CmdInvocation = reg.GetRegistryListTagsCmd(
			rc,
			sc.UseServiceAccount,
			string(imgName))
		return &sp
	}
	sc.ReadDigestsAndTags(mkRegistryListTagsCmd)

	sc.Info(sc.Inv.PrettyValue())

	// Promote.
	mkPromotionCmd := func(
		srcRegistry reg.RegistryName,
		destRC reg.RegistryContext,
		imageName reg.ImageName,
		digest reg.Digest, tag reg.Tag, tp reg.TagOp) stream.Producer {
		var sp stream.Subprocess
		sp.CmdInvocation = reg.GetWriteCmd(
			destRC,
			sc.UseServiceAccount,
			srcRegistry,
			imageName,
			digest,
			tag,
			tp)
		return &sp
	}

	exitCode := sc.Promote(mfest, mkPromotionCmd, nil)

	if *garbageCollectPtr {
		sc.Infof("---------- BEGIN GARBAGE COLLECTION: %s ----------\n",
			*manifestPtr)
		// Re-read the state of the world.
		sc.ReadImageNames(mkRegistryListingCmd)
		sc.ReadDigestsAndTags(mkRegistryListTagsCmd)
		// Garbage-collect all untagged images in dest registry.
		mkTagDeletionCmd := func(
			dest reg.RegistryContext,
			imageName reg.ImageName,
			digest reg.Digest) stream.Producer {
			var sp stream.Subprocess
			sp.CmdInvocation = reg.GetDeleteCmd(
				dest,
				sc.UseServiceAccount,
				imageName,
				digest)
			return &sp
		}
		sc.GarbageCollect(mfest, mkTagDeletionCmd, nil)
	}

	if *dryRunPtr {
		fmt.Printf("********** FINISHED (DRY RUN): %s **********\n",
			*manifestPtr)
	} else {
		fmt.Printf("********** FINISHED: %s **********\n", *manifestPtr)
	}
	os.Exit(exitCode)
}

func printVersion() {
	fmt.Printf("Built:   %s\n", TimestampUtcRfc3339)
	fmt.Printf("Version: %s\n", GitDescribe)
	fmt.Printf("Commit:  %s\n", GitCommit)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}
