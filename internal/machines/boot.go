package machines

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ubuntu/zsys/internal/config"
	"github.com/ubuntu/zsys/internal/i18n"
	"github.com/ubuntu/zsys/internal/log"
	"github.com/ubuntu/zsys/internal/zfs"
	"github.com/ubuntu/zsys/internal/zfs/libzfs"
)

// EnsureBoot consolidates canmount states for early boot.
// It will create any needed clones and userdata if required.
// A transactional zfs element should be passed to optionally revert if an error is returned (only part of the datasets
// were changed).
// We ensure that we don't modify any existing tags (those will be done in commit()) so that failing boots didn't modify
// the system, apart for canmount auto/on which are consolidated unconditionally on each boot anyway.
// Note that a rescan if performed if any modifications change the dataset layout. However, until ".Commit()" is called,
// machine.current will return the correct machine, but the main dataset switch won't be done. This allows us here and
// in .Commit()
// Return if any dataset / machine changed has been done during boot and an error if any encountered.
// TODO: propagate error to user graphically
func (ms *Machines) EnsureBoot(ctx context.Context) (bool, error) {
	if !ms.current.isZsys() {
		log.Info(ctx, i18n.G("Current machine isn't Zsys, nothing to do on boot"))
		return false, nil
	}

	t, cancel := ms.z.NewTransaction(ctx)
	defer t.Done()

	root, revertUserData := bootParametersFromCmdline(ms.cmdline)
	m, bootedState := ms.findFromRoot(root)
	log.Infof(ctx, i18n.G("Ensure boot on %q"), root)

	bootedOnSnapshot := hasBootedOnSnapshot(ms.cmdline)
	// We are creating new clones (bootfs and optionnally, userdata) if wasn't promoted already
	if bootedOnSnapshot && ms.current.ID != bootedState.ID {
		log.Infof(ctx, i18n.G("Booting on snapshot: %q cloned to %q\n"), root, bootedState.ID)

		// We skip it if we booted on a snapshot with userdatasets already created. This would mean that EnsureBoot
		// was called twice before Commit() during this boot. A new boot will create a new suffix id, so we won't block
		// the machine forever in case of a real issue.
		needCreateUserDatas := revertUserData && !(bootedOnSnapshot && len(bootedState.getUsersDatasets()) > 0)
		if err := m.History[root].createClones(t, bootedState.ID, needCreateUserDatas); err != nil {
			cancel()
			return false, err
		}

		if err := ms.Refresh(ctx); err != nil {
			return false, err
		}
		m, bootedState = ms.findFromRoot(root)
	}

	// We don't revert userdata, so we are using main state machine userdata to keep on the same track.
	// It's a no-op if the active state was the main one already.
	// In case of system revert (either from cloning or rebooting this older dataset without user data revert), the newly
	// active state won't be the main one, and so, we only take its main state userdata.
	if !revertUserData {
		bootedState.Users = m.Users
	}

	// Start switching every non desired system and user datasets to noauto
	var systemDatasets []*zfs.Dataset
	for _, ds := range bootedState.Datasets {
		systemDatasets = append(systemDatasets, ds...)
	}
	noAutoDatasets := diffDatasets(ms.allSystemDatasets, systemDatasets)
	userDatasets := bootedState.getUsersDatasets()

	noAutoDatasets = append(noAutoDatasets, diffDatasets(ms.allUsersDatasets, userDatasets)...)
	hasChanges, err := switchDatasetsCanMount(t, noAutoDatasets, "noauto")
	if err != nil {
		cancel()
		return false, err
	}

	// Switch current state system and user datasets to on
	autoDatasets := append(systemDatasets, userDatasets...)
	ok, err := switchDatasetsCanMount(t, autoDatasets, "on")
	if err != nil {
		cancel()
		return false, err
	}

	if ok || hasChanges {
		hasChanges = true
		if err := ms.Refresh(ctx); err != nil {
			return false, err
		}
	}

	return hasChanges, nil
}

// Commit current state to be the active one by promoting its datasets if needed, set last used,
// associate user datasets to it and rebuilding grub menu.
// After this operation, every New() call will get the current and correct system state.
// Return if any dataset / machine changed has been done during boot commit and an error if any encountered.
func (ms *Machines) Commit(ctx context.Context) (bool, error) {
	if !ms.current.isZsys() {
		log.Info(ctx, i18n.G("Current machine isn't Zsys, nothing to commit on boot"))
		return false, nil
	}

	t, cancel := ms.z.NewTransaction(ctx)
	defer t.Done()

	root, revertUserData := bootParametersFromCmdline(ms.cmdline)
	m, bootedState := ms.findFromRoot(root)
	log.Infof(ctx, i18n.G("Committing boot for %q"), root)

	// Get user datasets. As we didn't tag the user datasets and promote the system one, the machines doesn't correspond
	// to the reality.

	// We don't revert userdata, so we are using main state machine userdata to keep on the same track.
	// It's a no-op if the active state was the main one already.
	// In case of system revert (either from cloning or rebooting this older dataset without user data revert), the newly
	// active state won't be the main one, and so, we only take its main state userdata.
	if !revertUserData {
		bootedState.Users = m.Users
	}

	// Retag new userdatasets if needed
	userDatasets := bootedState.getUsersDatasets()
	if err := switchUsersDatasetsTags(t, bootedState.ID, ms.allUsersDatasets, userDatasets); err != nil {
		cancel()
		return false, err
	}

	var systemDatasets []*zfs.Dataset
	for _, ds := range bootedState.Datasets {
		systemDatasets = append(systemDatasets, ds...)
	}
	// System and users datasets: set lastUsed
	currentTime := strconv.Itoa(int(time.Now().Unix()))
	// Last used is not a relevant change for signalling a change and justify bootloader rebuild: last-used is not
	// displayed for current system dataset.
	log.Infof(ctx, i18n.G("set current time to %q"), currentTime)
	for _, d := range append(systemDatasets, userDatasets...) {
		if err := t.SetProperty(libzfs.LastUsedProp, currentTime, d.Name, false); err != nil {
			cancel()
			return false, fmt.Errorf(i18n.G("couldn't set last used time to %q: ")+config.ErrorFormat, currentTime, err)
		}
	}

	var changed bool

	kernel := kernelFromCmdline(ms.cmdline)
	log.Infof(ctx, i18n.G("Set latest booted kernel to %q\n"), kernel)
	if systemDatasets[0].LastBootedKernel != kernel {
		// Signal last booted kernel changes.
		// This will help the bootloader, like grub, to rebuild and adjust the marker for last successfully booted kernel in advanced options.
		changed = true
		if err := t.SetProperty(libzfs.LastBootedKernelProp, kernel, bootedState.ID, false); err != nil {
			cancel()
			return false, fmt.Errorf(i18n.G("couldn't set last booted kernel to %q ")+config.ErrorFormat, kernel, err)
		}
	}

	// Promotion needed for system and user datasets
	log.Info(ctx, i18n.G("Promoting user datasets"))
	chg, err := promoteDatasets(t, userDatasets)
	if err != nil {
		cancel()
		return false, err
	}
	changed = changed || chg
	log.Info(ctx, i18n.G("Promoting system datasets"))
	chg, err = promoteDatasets(t, systemDatasets)
	if err != nil {
		cancel()
		return false, err
	}
	changed = changed || chg

	if err := ms.Refresh(ctx); err != nil {
		return false, err
	}

	return changed, nil
}

// diffDatasets returns datasets in a that aren't in b
func diffDatasets(a, b []*zfs.Dataset) []*zfs.Dataset {
	mb := make(map[string]struct{}, len(b))
	for _, x := range b {
		mb[x.Name] = struct{}{}
	}
	var diff []*zfs.Dataset
	for _, x := range a {
		if _, found := mb[x.Name]; !found {
			diff = append(diff, x)
		}
	}
	return diff
}

func (snapshot State) createClones(t *zfs.Transaction, bootedStateID string, needCreateUserDatas bool) error {
	// get current generated suffix by initramfs
	j := strings.LastIndex(bootedStateID, "_")
	if j < 0 || strings.HasSuffix(bootedStateID, "_") {
		return fmt.Errorf(i18n.G("Mounted clone bootFS dataset created by initramfs doesn't have a valid _suffix (at least .*_<onechar>): %q"), bootedStateID)
	}
	suffix := bootedStateID[j+1:]

	// Skip existing datasets in the cloning phase. We assume any error would mean that EnsureBoot was called twice
	// before Commit() during this boot. A new boot will create a new suffix id, so we won't block the machine forever
	// in case of a real issue.
	// Clone fails on system dataset already exists and skipping requested -> ok, other clone fails -> return error
	for route := range snapshot.Datasets {
		log.Infof(t.Context(), i18n.G("cloning %q and children"), route)
		if err := t.Clone(route, suffix, true, true); err != nil {
			return fmt.Errorf(i18n.G("Couldn't create new subdatasets from %q. Assuming it has already been created successfully: %v"), route, err)
		}
	}

	// Handle userdata by creating new clones in case a revert was requested
	if !needCreateUserDatas {
		return nil
	}

	log.Info(t.Context(), i18n.G("Reverting user data"))
	// Find user datasets attached to the snapshot and clone them
	// Only root datasets are cloned
	userDataSuffix := t.Zfs.GenerateID(6)
	for _, us := range snapshot.Users {
		// Recursively clones childrens, which shouldn't have bootfs elements.
		if err := t.Clone(us.ID, userDataSuffix, false, true); err != nil {
			return fmt.Errorf(i18n.G("couldn't create new user datasets from %q: %v"), snapshot.ID, err)
		}
		// Associate this parent new user dataset to its parent system dataset
		base, _ := splitSnapshotName(us.ID)
		// Reformat the name with the new uuid and clone now the dataset.
		suffixIndex := strings.LastIndex(base, "_")
		userdatasetName := base[:suffixIndex] + "_" + userDataSuffix
		if err := t.SetProperty(libzfs.BootfsDatasetsProp, bootedStateID, userdatasetName, false); err != nil {
			return fmt.Errorf(i18n.G("couldn't add %q to BootfsDatasets property of %q: ")+config.ErrorFormat, bootedStateID, us.ID, err)
		}
	}

	return nil
}

func switchDatasetsCanMount(t *zfs.Transaction, ds []*zfs.Dataset, canMount string) (hasChanges bool, err error) {
	// Only handle on and noauto datasets, not off
	initialCanMount := "on"
	if canMount == "on" {
		initialCanMount = "noauto"
	}

	for _, d := range ds {
		if d.CanMount != initialCanMount || d.IsSnapshot {
			continue
		}
		log.Infof(t.Context(), i18n.G("Switch dataset %q to mount %q"), d.Name, canMount)
		if err := t.SetProperty(libzfs.CanmountProp, canMount, d.Name, false); err != nil {
			return false, fmt.Errorf(i18n.G("couldn't switch %q canmount property to %q: ")+config.ErrorFormat, d.Name, canMount, err)
		}
		hasChanges = true
	}

	return hasChanges, nil
}

// switchUsersDatasetsTags tags and untags users datasets to associate with current main system dataset id.
func switchUsersDatasetsTags(t *zfs.Transaction, id string, allUsersDatasets, currentUsersDatasets []*zfs.Dataset) error {
	// Untag non attached userdatasets
	for _, d := range diffDatasets(allUsersDatasets, currentUsersDatasets) {
		if d.IsSnapshot {
			continue
		}
		var newTag string
		// Multiple elements, strip current bootfs dataset name
		if d.BootfsDatasets != "" && d.BootfsDatasets != id {
			newTag = strings.Replace(d.BootfsDatasets, id+bootfsdatasetsSeparator, "", -1)
			newTag = strings.TrimSuffix(newTag, bootfsdatasetsSeparator+id)
		}
		if newTag == d.BootfsDatasets {
			continue
		}
		log.Infof(t.Context(), i18n.G("Untagging user dataset: %q"), d.Name)
		if err := t.SetProperty(libzfs.BootfsDatasetsProp, newTag, d.Name, false); err != nil {
			return fmt.Errorf(i18n.G("couldn't remove %q to BootfsDatasets property of %q: ")+config.ErrorFormat, id, d.Name, err)
		}
	}
	// Tag userdatasets to associate with this successful boot state, if wasn't tagged already
	// (case of no user data revert, associate with different previous main system)
	// TOREMOVE in 20.04 once compatible ubiquity is uploaded: && d.LastUsed != 0)
	// We want to transition to newer tag format com.ubuntu.zsys the first time we
	// set it.
	for _, d := range currentUsersDatasets {
		if (d.BootfsDatasets == id && d.LastUsed != 0) ||
			strings.Contains(d.BootfsDatasets, id+bootfsdatasetsSeparator) ||
			strings.HasSuffix(d.BootfsDatasets, bootfsdatasetsSeparator+id) {
			continue
		}
		log.Infof(t.Context(), i18n.G("Tag current user dataset: %q"), d.Name)
		newTag := d.BootfsDatasets + bootfsdatasetsSeparator + id
		// TOREMOVE in 20.04: this double check as well (due to && d.LastUsed != 0)
		if d.BootfsDatasets == id && d.LastUsed == 0 {
			newTag = d.BootfsDatasets
		}
		if err := t.SetProperty(libzfs.BootfsDatasetsProp, newTag, d.Name, false); err != nil {
			return fmt.Errorf(i18n.G("couldn't add %q to BootfsDatasets property of %q: ")+config.ErrorFormat, id, d.Name, err)
		}
	}

	return nil
}

func promoteDatasets(t *zfs.Transaction, ds []*zfs.Dataset) (changed bool, err error) {
	for _, d := range ds {
		// Even if we already check for this in Promote(), do an origin check here to only set changed to true
		// when needed.
		if d.Origin == "" {
			continue
		}
		changed = true
		log.Infof(t.Context(), i18n.G("Promoting dataset: %q"), d.Name)
		if err := t.Promote(d.Name); err != nil {
			return false, fmt.Errorf(i18n.G("couldn't promote dataset %q: ")+config.ErrorFormat, d.Name, err)
		}
	}

	return changed, nil
}
