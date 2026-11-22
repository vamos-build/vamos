// Package testenv supplies shared project fixtures and build prerequisites.
//
// # Invariants
//
// Helpers exercise real filesystem implementations and buildah, rather than replacing build behavior.
// In-memory projects use a stable logical root;
// disk projects keep host paths separate from paths visible through afero.
//
// # Changes
//
// Use Project for in-memory fixtures and DiskProject when a test needs symlinks or writes to the project.
// Main in main.go is the TestMain of packages whose tests build.
// NewNonce gives values new to each call, so that actions run rather than come from the cache.
// WalkImage and ImagePath in image.go read a saved image's flattened filesystem.
// SuiteStores and PrivateStores in stores.go give tests container stores and build caches of their own,
// so that no test touches the developer's; see RemoveSuiteStores for cleanup.
// PrivateStores copies in the overlay driver's checks of the filesystem that the suite's store made,
// so that tests running in parallel don't each fork the binary to check it, as seedDriverChecks says.
// Their overlay driver diffs layers natively, without the metacopy of Fedora's default storage.conf,
// whose naive diffs wait for the next whole second after every commit;
// NaiveDiffEnv gives a test binary's stores metacopy, as TestNaiveAndNativeDiffsMakeTheSameImage in cli does.
// Their UnnamedImages lists what a failed build may leave in the store.
// RequireBuildah and RequireOwnership probe for what a test needs and skip it when that is missing,
// or fail it when RequireIntegrationEnv says the suite must run whole;
// they do not configure privileges, isolation or networking.
// Tests in several packages use EditedFs to edit sources during a read, and DeniedFs to refuse to check a file.
// Give each test its own EditedFs, since it keeps a count of opens to decide when to edit.
// A test that starts a worker gives it CgroupRoot, as worker.Config's or cli.Config's CgroupRoot,
// or its start may kill the attempts of a worker in another test that runs at once, in its binary or another.
// InsecureRegistry and MirroredRegistry add a test's registries to the registries.conf that Main shares with the
// binary's tests for the whole run, so that tests running in parallel never change the environment.
// Main mirrors Docker Hub for pulls by digest, in hub.go, keeping what it fetches in the user's cache folder,
// or HubCacheEnv's, for later runs, so that pinned pulls of busybox don't spend Docker Hub's pull allowance,
// in a registries.conf read after the environment's, unless that configures Docker Hub itself.
// InsecureRegistry in registry.go lets an executor reach a test's registry over plain HTTP,
// and FakeDocker in docker.go serves what loading an image into Docker uses.
// Unsetenv unsets an environment variable for the rest of a test.
// Find the behavior specifications, and the steps they are written in, in internal/features.
package testenv
