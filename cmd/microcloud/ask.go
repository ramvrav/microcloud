package main

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	cli "github.com/canonical/lxd/shared/cmd"
	"github.com/canonical/lxd/shared/logger"
	"github.com/canonical/lxd/shared/units"
	"github.com/canonical/lxd/shared/validate"
	cephTypes "github.com/canonical/microceph/microceph/api/types"

	"github.com/canonical/microcloud/microcloud/api/types"
	"github.com/canonical/microcloud/microcloud/mdns"
	"github.com/canonical/microcloud/microcloud/service"
)

// askRetry will print all errors and re-attempt the given function on user input.
func (c *initConfig) askRetry(question string, f func() error) {
	for {
		retry := false
		err := f()
		if err != nil {
			fmt.Println(err)

			if !c.autoSetup {
				retry, err = c.asker.AskBool(fmt.Sprintf("%s (yes/no) [default=yes]: ", question), "yes")
				if err != nil {
					fmt.Println(err)
					retry = false
				}
			}
		}

		if !retry {
			break
		}
	}
}

func (c *initConfig) askMissingServices(services []types.ServiceType, stateDirs map[types.ServiceType]string) ([]types.ServiceType, error) {
	missingServices := []string{}
	for serviceType, stateDir := range stateDirs {
		if service.Exists(serviceType, stateDir) {
			services = append(services, serviceType)
		} else {
			missingServices = append(missingServices, string(serviceType))
		}
	}

	if len(missingServices) > 0 {
		serviceStr := strings.Join(missingServices, ", ")
		if !c.autoSetup {
			confirm, err := c.asker.AskBool(fmt.Sprintf("%s not found. Continue anyway? (yes/no) [default=yes]: ", serviceStr), "yes")
			if err != nil {
				return nil, err
			}

			if !confirm {
				return nil, fmt.Errorf("User aborted")
			}

			return services, nil
		}

		logger.Infof("Skipping %s (could not detect service state directory)", serviceStr)
	}

	return services, nil
}

func (c *initConfig) askAddress() error {
	info, err := mdns.GetNetworkInfo()
	if err != nil {
		return fmt.Errorf("Failed to find network interfaces: %w", err)
	}

	listenAddr := c.address
	if listenAddr == "" {
		if len(info) == 0 {
			return fmt.Errorf("Found no valid network interfaces")
		}

		listenAddr = info[0].Address
		if !c.autoSetup && len(info) > 1 {
			data := make([][]string, 0, len(info))
			for _, net := range info {
				data = append(data, []string{net.Address, net.Interface.Name})
			}

			table := NewSelectableTable([]string{"ADDRESS", "IFACE"}, data)
			c.askRetry("Retry selecting an address?", func() error {
				fmt.Println("Select an address for MicroCloud's internal traffic:")
				err := table.Render(table.rows)
				if err != nil {
					return err
				}

				answers, err := table.GetSelections()
				if err != nil {
					return err
				}

				if len(answers) != 1 {
					return fmt.Errorf("You must select exactly one address")
				}

				listenAddr = table.SelectionValue(answers[0], "ADDRESS")

				fmt.Printf(" Using address %q for MicroCloud\n\n", listenAddr)

				return nil
			})
		} else {
			fmt.Printf("Using address %q for MicroCloud\n", listenAddr)
		}
	}

	var subnet *net.IPNet
	var iface *net.Interface
	for _, network := range info {
		if network.Subnet.Contains(net.ParseIP(listenAddr)) {
			subnet = network.Subnet
			iface = &network.Interface
			break
		}
	}

	if subnet == nil {
		return fmt.Errorf("Cloud not find valid subnet for address %q", listenAddr)
	}

	if !c.autoSetup {
		filter, err := c.asker.AskBool(fmt.Sprintf("Limit search for other MicroCloud servers to %s? (yes/no) [default=yes]: ", subnet.String()), "yes")
		if err != nil {
			return err
		}

		if !filter {
			subnet = nil
		}
	}

	c.address = listenAddr
	c.lookupIface = iface
	c.lookupSubnet = subnet

	return nil
}

func (c *CmdControl) askDisks(sh *service.Handler, systems map[string]InitSystem, autoSetup bool, wipeAllDisks bool, encryptAllDisks bool) error {
	_, bootstrap := systems[sh.Name]
	allResources := make(map[string]*api.Resources, len(systems))
	var err error
	for peer, system := range systems {
		allResources[peer], err = sh.Services[types.LXD].(*service.LXDService).GetResources(context.Background(), peer, system.ServerInfo.Address, system.ServerInfo.AuthSecret)
		if err != nil {
			return fmt.Errorf("Failed to get system resources of peer %q: %w", peer, err)
		}
	}

	foundDisks := false
	for peer, r := range allResources {
		system := systems[peer]
		system.AvailableDisks = make([]api.ResourcesStorageDisk, 0, len(r.Storage.Disks))
		for _, disk := range r.Storage.Disks {
			if len(disk.Partitions) == 0 {
				system.AvailableDisks = append(system.AvailableDisks, disk)
			}
		}

		if len(system.AvailableDisks) > 0 {
			foundDisks = true
		}

		systems[peer] = system
	}

	wantsDisks := true
	if !autoSetup && foundDisks {
		wantsDisks, err = c.asker.AskBool("Would you like to set up local storage? (yes/no) [default=yes]: ", "yes")
		if err != nil {
			return err
		}
	}

	if !foundDisks {
		wantsDisks = false
	}

	lxd := sh.Services[types.LXD].(*service.LXDService)
	if wantsDisks {
		c.askRetry("Retry selecting disks?", autoSetup, func() error {
			return askLocalPool(systems, autoSetup, wipeAllDisks, *lxd)
		})
	}

	if sh.Services[types.MicroCeph] != nil {
		availableDisks := map[string][]api.ResourcesStorageDisk{}
		for peer, system := range systems {
			if len(system.AvailableDisks) > 0 {
				availableDisks[peer] = system.AvailableDisks
			}
		}

		if bootstrap && len(availableDisks) < 3 {
			fmt.Println("Insufficient number of disks available to set up distributed storage, skipping at this time")
		} else {
			wantsDisks = true
			if !autoSetup {
				wantsDisks, err = c.asker.AskBool("Would you like to set up distributed storage? (yes/no) [default=yes]: ", "yes")
				if err != nil {
					return err
				}

				if len(systems) != len(availableDisks) && wantsDisks {
					wantsDisks, err = c.asker.AskBool("Unable to find disks on some systems. Continue anyway? (yes/no) [default=yes]: ", "yes")
					if err != nil {
						return err
					}
				}
			}

			if wantsDisks {
				c.askRetry("Retry selecting disks?", autoSetup, func() error {
					return c.askRemotePool(systems, autoSetup, wipeAllDisks, encryptAllDisks, sh)
				})
			}
		}
	}

	if !bootstrap {
		for peer, system := range systems {
			if len(system.MicroCephDisks) > 0 {
				if system.JoinConfig == nil {
					system.JoinConfig = []api.ClusterMemberConfigKey{}
				}

				system.JoinConfig = append(system.JoinConfig, lxd.DefaultCephStoragePoolJoinConfig())

				systems[peer] = system
			}
		}
	}

	return nil
}

func parseDiskPath(disk api.ResourcesStorageDisk) string {
	devicePath := fmt.Sprintf("/dev/%s", disk.ID)
	if disk.DeviceID != "" {
		devicePath = fmt.Sprintf("/dev/disk/by-id/%s", disk.DeviceID)
	} else if disk.DevicePath != "" {
		devicePath = fmt.Sprintf("/dev/disk/by-path/%s", disk.DevicePath)
	}

	return devicePath
}

func (c *initConfig) askLocalPool(sh *service.Handler) error {
	useJoinConfig := false
	askSystems := map[string]bool{}
	for _, info := range c.state {
		hasPool, supportsPool := info.SupportsLocalPool()
		if !supportsPool {
			logger.Warn("Skipping local storage pool setup, some systems don't support it")
			return nil
		}

		if hasPool {
			useJoinConfig = true
		} else {
			askSystems[info.ClusterName] = true
		}
	}

	availableDisks := map[string]map[string]api.ResourcesStorageDisk{}
	for name, state := range c.state {
		if len(state.AvailableDisks) == 0 {
			logger.Infof("Skipping local storage pool creation, peer %q has too few disks", name)

			return nil
		}

		if askSystems[name] {
			availableDisks[name] = state.AvailableDisks
		}
	}

	// Local storage is already set up on every system.
	if len(askSystems) == 0 {
		return nil
	}

	// We can't setup a local pool if not every system has a disk.
	if len(availableDisks) != len(askSystems) {
		return nil
	}

	data := [][]string{}
	selectedDisks := map[string]string{}
	for peer, disks := range availableDisks {
		// In auto mode, if there's no spare disk, then we can't add a remote storage pool, so skip local pool creation.
		if c.autoSetup && len(disks) < 2 {
			logger.Infof("Skipping local storage pool creation, peer %q has too few disks", peer)

			return nil
		}

		sortedDisks := []api.ResourcesStorageDisk{}
		for _, disk := range disks {
			sortedDisks = append(sortedDisks, disk)
		}

		sort.Slice(sortedDisks, func(i, j int) bool {
			return parseDiskPath(sortedDisks[i]) < parseDiskPath(sortedDisks[j])
		})

		for _, disk := range sortedDisks {
			devicePath := parseDiskPath(disk)
			data = append(data, []string{peer, disk.Model, units.GetByteSizeStringIEC(int64(disk.Size), 2), disk.Type, devicePath})

			// Add the first disk for each peer.
			if c.autoSetup {
				_, ok := selectedDisks[peer]
				if !ok {
					selectedDisks[peer] = devicePath
				}
			}
		}
	}

	var err error
	wantsDisks := true
	if !c.autoSetup {
		wantsDisks, err = c.asker.AskBool("Would you like to set up local storage? (yes/no) [default=yes]: ", "yes")
		if err != nil {
			return err
		}
	}

	if !wantsDisks {
		return nil
	}

	lxd := sh.Services[types.LXD].(*service.LXDService)
	toWipe := map[string]string{}
	wipeable, err := lxd.HasExtension(context.Background(), lxd.Name(), lxd.Address(), "", "storage_pool_source_wipe")
	if err != nil {
		return fmt.Errorf("Failed to check for source.wipe extension: %w", err)
	}

	if !c.autoSetup {
		c.askRetry("Retry selecting disks?", func() error {
			selected := map[string]string{}
			sort.Sort(cli.SortColumnsNaturally(data))
			header := []string{"LOCATION", "MODEL", "CAPACITY", "TYPE", "PATH"}
			table := NewSelectableTable(header, data)
			fmt.Println("Select exactly one disk from each cluster member:")
			err := table.Render(table.rows)
			if err != nil {
				return err
			}

			selectedRows, err := table.GetSelections()
			if err != nil {
				return fmt.Errorf("Failed to confirm local LXD disk selection: %w", err)
			}

			if len(selectedRows) == 0 {
				return fmt.Errorf("No disks selected")
			}

			for _, entry := range selectedRows {
				target := table.SelectionValue(entry, "LOCATION")
				path := table.SelectionValue(entry, "PATH")

				_, ok := selected[target]
				if ok {
					return fmt.Errorf("Failed to add local storage pool: Selected more than one disk for target peer %q", target)
				}

				selected[target] = path
			}

			if len(selected) != len(askSystems) {
				return fmt.Errorf("Failed to add local storage pool: Some peers don't have an available disk")
			}

			if !c.wipeAllDisks && wipeable {
				fmt.Println("Select which disks to wipe:")
				err := table.Render(selectedRows)
				if err != nil {
					return err
				}

				wipeRows, err := table.GetSelections()
				if err != nil {
					return fmt.Errorf("Failed to confirm which disks to wipe: %w", err)
				}

				for _, entry := range wipeRows {
					target := table.SelectionValue(entry, "LOCATION")
					path := table.SelectionValue(entry, "PATH")
					toWipe[target] = path
				}
			}

			selectedDisks = selected

			return nil
		})
	}

	if len(selectedDisks) == 0 {
		return nil
	}

	if c.wipeAllDisks && wipeable {
		toWipe = selectedDisks
	}

	var joinConfigs map[string][]api.ClusterMemberConfigKey
	var finalConfigs []api.StoragePoolsPost
	var targetConfigs map[string][]api.StoragePoolsPost
	if useJoinConfig {
		joinConfigs = map[string][]api.ClusterMemberConfigKey{}
		for target, path := range selectedDisks {
			joinConfigs[target] = lxd.DefaultZFSStoragePoolJoinConfig(wipeable && toWipe[target] != "", path)
		}
	} else {
		targetConfigs = map[string][]api.StoragePoolsPost{}
		for target, path := range selectedDisks {
			targetConfigs[target] = []api.StoragePoolsPost{lxd.DefaultPendingZFSStoragePool(wipeable && toWipe[target] != "", path)}
		}

		if len(targetConfigs) > 0 {
			finalConfigs = []api.StoragePoolsPost{lxd.DefaultZFSStoragePool()}
		}
	}

	for target, path := range selectedDisks {
		fmt.Printf(" Using %q on %q for local storage pool\n", path, target)
	}

	if len(selectedDisks) > 0 {
		// Add a space between the CLI and the response.
		fmt.Println("")
	}

	newAvailableDisks := map[string]map[string]api.ResourcesStorageDisk{}
	for target, path := range selectedDisks {
		newAvailableDisks[target] = map[string]api.ResourcesStorageDisk{}
		for id, disk := range availableDisks[target] {
			if parseDiskPath(disk) != path {
				newAvailableDisks[target][id] = disk
			}
		}
	}

	for peer, system := range c.systems {
		if !askSystems[peer] {
			continue
		}

		if system.JoinConfig == nil {
			system.JoinConfig = []api.ClusterMemberConfigKey{}
		}

		if system.TargetStoragePools == nil {
			system.TargetStoragePools = []api.StoragePoolsPost{}
		}

		if system.StoragePools == nil {
			system.StoragePools = []api.StoragePoolsPost{}
		}

		if joinConfigs[peer] != nil {
			system.JoinConfig = append(system.JoinConfig, joinConfigs[peer]...)
		}

		if targetConfigs[peer] != nil {
			system.TargetStoragePools = append(system.TargetStoragePools, targetConfigs[peer]...)
		}

		if peer == sh.Name && finalConfigs != nil {
			system.StoragePools = append(system.StoragePools, finalConfigs...)
		}

		c.systems[peer] = system
	}

	for peer, state := range c.state {
		if askSystems[peer] {
			state.AvailableDisks = newAvailableDisks[peer]
			c.state[peer] = state
		}
	}

	return nil
}

func validateCephInterfacesForSubnet(lxdService *service.LXDService, systems map[string]InitSystem, availableCephNetworkInterfaces map[string][]service.CephDedicatedInterface, askedCephSubnet string) error {
	validatedCephInterfacesData, err := lxdService.ValidateCephInterfaces(askedCephSubnet, availableCephNetworkInterfaces)
	if err != nil {
		return err
	}

	// List the detected network interfaces
	for _, interfaces := range validatedCephInterfacesData {
		for _, iface := range interfaces {
			fmt.Printf("Interface %q (%q) detected on cluster member %q\n", iface[1], iface[2], iface[0])
		}
	}

	// Even though not all the cluster members might have OSDs,
	// we check that all the machines have at least one interface to sustain the Ceph network
	for systemName := range systems {
		if len(validatedCephInterfacesData[systemName]) == 0 {
			return fmt.Errorf("Not enough network interfaces found with an IP within the given CIDR subnet on %q.\nYou need at least one interface per cluster member.", systemName)
		}
	}

	return nil
}

// getTargetCephNetworks fetches the Ceph network configuration from the existing Ceph cluster.
// If the system passed as an argument is nil, we will fetch the local Ceph network configuration.
func getTargetCephNetworks(sh *service.Handler, s *InitSystem) (internalCephNetwork *net.IPNet, err error) {
	microCephService := sh.Services[types.MicroCeph].(*service.CephService)
	if microCephService == nil {
		return nil, fmt.Errorf("failed to get MicroCeph service")
	}

	var cephAddr string
	var cephAuthSecret string
	if s != nil && s.ServerInfo.Name != sh.Name {
		cephAddr = s.ServerInfo.Address
		cephAuthSecret = s.ServerInfo.AuthSecret
	}

	remoteCephConfigs, err := microCephService.ClusterConfig(context.Background(), cephAddr, cephAuthSecret)
	if err != nil {
		return nil, err
	}

	for key, value := range remoteCephConfigs {
		if key == "cluster_network" && value != "" {
			// Sometimes, the default cluster_network value in the Ceph configuration
			// is not a network range but a regular IP address. We need to extract the network range.
			_, valueNet, err := net.ParseCIDR(value)
			if err != nil {
				return nil, fmt.Errorf("failed to parse the Ceph cluster network configuration from the existing Ceph cluster: %v", err)
			}

			internalCephNetwork = valueNet
		}
	}

	return internalCephNetwork, nil
}

func (c *CmdControl) askRemotePool(systems map[string]InitSystem, autoSetup bool, wipeAllDisks bool, encryptAllDisks bool, sh *service.Handler) error {
	header := []string{"LOCATION", "MODEL", "CAPACITY", "TYPE", "PATH"}
	data := [][]string{}
	for peer, system := range systems {
		sortedDisks := make([]api.ResourcesStorageDisk, 0, len(system.AvailableDisks))
		sortedDisks = append(sortedDisks, system.AvailableDisks...)
		sort.Slice(sortedDisks, func(i, j int) bool {
			return parseDiskPath(sortedDisks[i]) < parseDiskPath(sortedDisks[j])
		})

		for _, disk := range sortedDisks {
			// Skip any disks that have been reserved for the local storage pool.
			devicePath := parseDiskPath(disk)
			data = append(data, []string{peer, disk.Model, units.GetByteSizeStringIEC(int64(disk.Size), 2), disk.Type, devicePath})
		}
	}

	if len(data) == 0 {
		return fmt.Errorf("Found no available disks")
	}

	sort.Sort(cli.SortColumnsNaturally(data))
	table := NewSelectableTable(header, data)
	selected := table.rows
	var toWipe []string
	if wipeAllDisks {
		toWipe = selected
	}

	var toEncrypt []string
	if encryptAllDisks {
		toEncrypt = selected
	}

	if len(table.rows) == 0 {
		return nil
	}

	if !autoSetup {
		fmt.Println("Select from the available unpartitioned disks:")
		err := table.Render(table.rows)
		if err != nil {
			return err
		}

		selected, err = table.GetSelections()
		if err != nil {
			return fmt.Errorf("Failed to confirm disk selection: %w", err)
		}

		if len(selected) > 0 && !wipeAllDisks {
			fmt.Println("Select which disks to wipe:")
			err := table.Render(selected)
			if err != nil {
				return err
			}

			toWipe, err = table.GetSelections()
			if err != nil {
				return fmt.Errorf("Failed to confirm disk wipe selection: %w", err)
			}
		}

		if len(selected) > 0 && !encryptAllDisks {
			encryptDisks, err := c.asker.AskBool("Do you want to encrypt the selected disks? (yes/no) [default=no]: ", "no")
			if err != nil {
				return err
			}

			if encryptDisks {
				toEncrypt = selected
			}
		}
	}

	wipeMap := make(map[string]bool, len(toWipe))
	for _, entry := range toWipe {
		_, ok := table.data[entry]
		if ok {
			wipeMap[entry] = true
		}
	}

	encryptMap := make(map[string]bool, len(toEncrypt))
	for _, entry := range toEncrypt {
		_, ok := table.data[entry]
		if ok {
			encryptMap[entry] = true
		}
	}

	diskCount := 0
	lxd := sh.Services[types.LXD].(*service.LXDService)
	for _, entry := range selected {
		target := table.SelectionValue(entry, "LOCATION")
		path := table.SelectionValue(entry, "PATH")
		system := systems[target]

		if system.MicroCephDisks == nil {
			diskCount++
			system.MicroCephDisks = []cephTypes.DisksPost{}
		}

		system.MicroCephDisks = append(
			system.MicroCephDisks,
			cephTypes.DisksPost{
				Path:    []string{path},
				Wipe:    wipeMap[entry],
				Encrypt: encryptMap[entry],
			},
		)

		systems[target] = system
	}

	if diskCount > 0 {
		for target, system := range systems {
			if system.TargetStoragePools == nil {
				system.TargetStoragePools = []api.StoragePoolsPost{}
			}

			_, bootstrap := systems[sh.Name]
			if bootstrap {
				system.TargetStoragePools = append(system.TargetStoragePools, lxd.DefaultPendingCephStoragePool())
				if target == sh.Name {
					if system.StoragePools == nil {
						system.StoragePools = []api.StoragePoolsPost{}
					}

					system.StoragePools = append(system.StoragePools, lxd.DefaultCephStoragePool())
				}
			}

			systems[target] = system
		}
	}

	_, checkMinSize := systems[sh.Name]
	if checkMinSize && diskCount < 3 {
		return fmt.Errorf("Unable to add remote storage pool: At least 3 peers must have allocated disks")
	}

	// Print a summary of what was chosen in this step.
	if diskCount > 0 {
		for peer, system := range systems {
			if len(system.MicroCephDisks) > 0 {
				fmt.Printf(" Using %d disk(s) on %q for remote storage pool\n", len(system.MicroCephDisks), peer)
			}
		}

		// Add a space between the CLI and the response.
		fmt.Println("")
	}

	setupCephFS := false
	_, bootstrap := systems[sh.Name]
	if bootstrap && !autoSetup {
		var err error
		ext := "storage_cephfs_create_missing"
		hasCephFS, err := lxd.HasExtension(context.Background(), lxd.Name(), lxd.Address(), "", ext)
		if err != nil {
			return fmt.Errorf("Failed to check for the %q LXD API extension: %w", ext, err)
		}

		if hasCephFS {
			setupCephFS, err = c.asker.AskBool("Would you like to set up CephFS remote storage? (yes/no) [default=yes]: ", "yes")
			if err != nil {
				return err
			}
		}
	}

	if !bootstrap {
		d, err := sh.Services[types.LXD].(*service.LXDService).Client(context.Background(), "")
		if err != nil {
			return err
		}

		pools, err := d.GetStoragePools()
		if err != nil {
			return err
		}

		// If "cephfs" has been setup already, then set it up for the new system too.
		for _, pool := range pools {
			if pool.Driver == "cephfs" {
				setupCephFS = true
				break
			}
		}
	}

	if setupCephFS {
		for name, system := range systems {
			if bootstrap {
				system.TargetStoragePools = append(system.TargetStoragePools, lxd.DefaultPendingCephFSStoragePool())
				if sh.Name == name {
					system.StoragePools = append(system.StoragePools, lxd.DefaultCephFSStoragePool())
				}
			} else {
				system.JoinConfig = append(system.JoinConfig, lxd.DefaultCephFSStoragePoolJoinConfig())
			}

			systems[name] = system
		}
	}

	return nil
}

func (c *CmdControl) askProceedIfNoOverlayNetwork() error {
	proceedWithNoOverlayNetworking, err := c.asker.AskBool("FAN networking is not usable. Do you want to proceed with setting up an inoperable cluster? (yes/no) [default=no]: ", "no")
	if err != nil {
		return err
	}

	if proceedWithNoOverlayNetworking {
		return nil
	}

	return fmt.Errorf("cluster bootstrapping aborted due to lack of usable networking")
}

func (c *CmdControl) askNetwork(sh *service.Handler, systems map[string]InitSystem, microCloudInternalSubnet *net.IPNet, autoSetup bool) error {
	_, bootstrap := systems[sh.Name]
	lxd := sh.Services[types.LXD].(*service.LXDService)

	// Check if FAN networking is usable.
	fanUsable, _, err := lxd.FanNetworkUsable()
	if err != nil {
		return err
	}

	if fanUsable {
		for peer, system := range systems {
			if bootstrap {
				system.TargetNetworks = []api.NetworksPost{lxd.DefaultPendingFanNetwork()}
				if peer == sh.Name {
					network, err := lxd.DefaultFanNetwork()
					if err != nil {
						return err
					}

					system.Networks = []api.NetworksPost{network}
				}
			}

			systems[peer] = system
		}
	}

	// Automatic setup gets a basic fan setup.
	if autoSetup {
		if !fanUsable {
			return c.askProceedIfNoOverlayNetwork()
		}

		return nil
	}

	// Environments without OVN get a basic fan setup.
	if sh.Services[types.MicroOVN] == nil {
		if !fanUsable {
			return c.askProceedIfNoOverlayNetwork()
		}

		return nil
	}

	// Get the list of networks from all peers.
	infos := []mdns.ServerInfo{}
	for _, system := range systems {
		infos = append(infos, system.ServerInfo)
	}

	// Environments without Ceph don't need to configure the Ceph network.
	if sh.Services[types.MicroCeph] != nil {
		// Configure the Ceph networks.
		lxdService := sh.Services[types.LXD].(*service.LXDService)
		if lxdService == nil {
			return fmt.Errorf("failed to get LXD service")
		}

		availableCephNetworkInterfaces, err := lxdService.GetCephInterfaces(context.Background(), bootstrap, infos)
		if err != nil {
			return err
		}

		if len(availableCephNetworkInterfaces) == 0 {
			fmt.Println("No network interfaces found with IPs to set a dedicated Ceph network, skipping Ceph network setup")
		} else {
			if bootstrap {
				// First, check if there are any initialized systems for MicroCeph.
				var initializedMicroCephSystem *InitSystem
				for peer, system := range systems {
					if system.InitializedServices[types.MicroCeph][peer] != "" {
						initializedMicroCephSystem = &system
						break
					}
				}

				var customTargetCephInternalNetwork string
				if initializedMicroCephSystem != nil {
					// If there is at least one initialized system with MicroCeph (we consider that more than one initialized MicroCeph systems are part of the same cluster),
					// we need to fetch its Ceph configuration to validate against this to-be-bootstrapped cluster.
					targetInternalCephNetwork, err := getTargetCephNetworks(sh, initializedMicroCephSystem)
					if err != nil {
						return err
					}

					if targetInternalCephNetwork.String() != microCloudInternalSubnet.String() {
						customTargetCephInternalNetwork = targetInternalCephNetwork.String()
					}
				}

				microCloudInternalNetworkAddr := microCloudInternalSubnet.IP.Mask(microCloudInternalSubnet.Mask)
				ones, _ := microCloudInternalSubnet.Mask.Size()
				microCloudInternalNetworkAddrCIDR := fmt.Sprintf("%s/%d", microCloudInternalNetworkAddr.String(), ones)

				// If there is no remote Ceph cluster or is an existing remote Ceph has no configured networks
				// other than the default one (internal MicroCloud network), we ask the user to configure the Ceph networks.
				if customTargetCephInternalNetwork == "" {
					internalCephSubnet, err := c.asker.AskString(fmt.Sprintf("What subnet (either IPv4 or IPv6 CIDR notation) would you like your Ceph internal traffic on? [default: %s] ", microCloudInternalNetworkAddrCIDR), microCloudInternalNetworkAddrCIDR, validate.IsNetwork)
					if err != nil {
						return err
					}

					if internalCephSubnet != microCloudInternalNetworkAddrCIDR {
						err = validateCephInterfacesForSubnet(lxdService, systems, availableCephNetworkInterfaces, internalCephSubnet)
						if err != nil {
							return err
						}

						bootstrapSystem := systems[sh.Name]
						bootstrapSystem.MicroCephInternalNetworkSubnet = internalCephSubnet
						systems[sh.Name] = bootstrapSystem
					}
				} else {
					// Else, we validate that the systems to be bootstrapped comply with the network configuration of the existing remote Ceph cluster,
					// and set their Ceph network configuration accordingly.
					if customTargetCephInternalNetwork != "" && customTargetCephInternalNetwork != microCloudInternalNetworkAddrCIDR {
						err = validateCephInterfacesForSubnet(lxdService, systems, availableCephNetworkInterfaces, customTargetCephInternalNetwork)
						if err != nil {
							return err
						}

						bootstrapSystem := systems[sh.Name]
						bootstrapSystem.MicroCephInternalNetworkSubnet = customTargetCephInternalNetwork
						systems[sh.Name] = bootstrapSystem
					}
				}
			} else {
				// If we are not bootstrapping, we target the local MicroCeph and fetch its network cluster config.
				localInternalCephNetwork, err := getTargetCephNetworks(sh, nil)
				if err != nil {
					return err
				}

				if localInternalCephNetwork.String() != "" && localInternalCephNetwork.String() != microCloudInternalSubnet.String() {
					err = validateCephInterfacesForSubnet(lxd, systems, availableCephNetworkInterfaces, localInternalCephNetwork.String())
					if err != nil {
						return err
					}
				}
			}
		}
	}

	networks, err := sh.Services[types.LXD].(*service.LXDService).GetUplinkInterfaces(context.Background(), bootstrap, infos)
	if err != nil {
		return err
	}

	// Check if OVN is possible in the environment.
	canOVN := len(networks) > 0
	for _, nets := range networks {
		if len(nets) == 0 {
			canOVN = false
			break
		}
	}

	if !canOVN {
		fmt.Println("No dedicated uplink interfaces detected, skipping distributed networking")
		if !fanUsable {
			return c.askProceedIfNoOverlayNetwork()
		}

		return nil
	}

	// Ask the user if they want OVN.
	wantsOVN, err := c.asker.AskBool("Configure distributed networking? (yes/no) [default=yes]: ", "yes")
	if err != nil {
		return err
	}

	if !wantsOVN {
		return nil
	}

	missingSystems := len(systems) != len(networks)
	for _, nets := range networks {
		if len(nets) == 0 {
			missingSystems = true
			break
		}
	}

	if missingSystems {
		wantsSkip, err := c.asker.AskBool("Some systems are ineligible for distributed networking, which requires either an interface with no IPs assigned or a bridge. Continue anyway? (yes/no) [default=yes]: ", "yes")
		if err != nil {
			return err
		}

		if !wantsSkip {
			return nil
		}
	}

	// Uplink selection table.
	header := []string{"LOCATION", "IFACE", "TYPE"}
	fmt.Println("Select an available interface per system to provide external connectivity for distributed network(s):")
	data := [][]string{}
	for peer, nets := range networks {
		for _, net := range nets {
			data = append(data, []string{peer, net.Name, net.Type})
		}
	}

	table := NewSelectableTable(header, data)
	var selected map[string]string
	c.askRetry("Retry selecting uplink interfaces?", autoSetup, func() error {
		err := table.Render(table.rows)
		if err != nil {
			return err
		}

		answers, err := table.GetSelections()
		if err != nil {
			return err
		}

		selected = map[string]string{}
		for _, answer := range answers {
			target := table.SelectionValue(answer, "LOCATION")
			iface := table.SelectionValue(answer, "IFACE")

			if selected[target] != "" {
				return fmt.Errorf("Failed to add OVN uplink network: Selected more than one interface for target %q", target)
			}

			selected[target] = iface
		}

		if len(selected) != len(networks) {
			return fmt.Errorf("Failed to add OVN uplink network: Some peers don't have a selected interface")
		}

		return nil
	})

	for peer, iface := range selected {
		fmt.Printf(" Using %q on %q for OVN uplink\n", iface, peer)
	}

	// If we didn't select anything, then abort network setup.
	if len(selected) == 0 {
		return nil
	}

	// Add a space between the CLI and the response.
	fmt.Println("")

	// Prepare the configuration.
	var dnsAddresses string
	ipConfig := map[string]string{}
	if bootstrap {
		for _, ip := range []string{"IPv4", "IPv6"} {
			validator := func(s string) error {
				if s == "" {
					return nil
				}

				addr, _, err := net.ParseCIDR(s)
				if err != nil {
					return err
				}

				if addr.To4() == nil && ip == "IPv4" {
					return fmt.Errorf("Not a valid IPv4")
				}

				if addr.To4() != nil && ip == "IPv6" {
					return fmt.Errorf("Not a valid IPv6")
				}

				return nil
			}

			msg := fmt.Sprintf("Specify the %s gateway (CIDR) on the uplink network (empty to skip %s): ", ip, ip)
			gateway, err := c.asker.AskString(msg, "", validator)
			if err != nil {
				return err
			}

			if gateway != "" {
				if ip == "IPv4" {
					rangeStart, err := c.asker.AskString(fmt.Sprintf("Specify the first %s address in the range to use on the uplink network: ", ip), "", validate.Required(validate.IsNetworkAddressV4))
					if err != nil {
						return err
					}

					rangeEnd, err := c.asker.AskString(fmt.Sprintf("Specify the last %s address in the range to use on the uplink network: ", ip), "", validate.Required(validate.IsNetworkAddressV4))
					if err != nil {
						return err
					}

					ipConfig[gateway] = fmt.Sprintf("%s-%s", rangeStart, rangeEnd)
				} else {
					ipConfig[gateway] = ""
				}
			}
		}

		gateways := []string{}
		for gateway := range ipConfig {
			gatewayAddr, _, err := net.ParseCIDR(gateway)
			if err != nil {
				return err
			}

			gateways = append(gateways, gatewayAddr.String())
		}

		gatewayAddrs := strings.Join(gateways, ",")
		dnsAddresses, err = c.asker.AskString(fmt.Sprintf("Specify the DNS addresses (comma-separated IPv4 / IPv6 addresses) for the distributed network (default: %s): ", gatewayAddrs), gatewayAddrs, validate.Optional(validate.IsListOf(validate.IsNetworkAddress)))
		if err != nil {
			return err
		}
	}

	// If interfaces were selected for OVN, remove the FAN config.
	if len(selected) > 0 {
		for peer, system := range systems {
			system.TargetNetworks = []api.NetworksPost{}
			system.Networks = []api.NetworksPost{}

			systems[peer] = system
		}
	}

	// If we are adding a new member, a MemberConfig entry should suffice to create the network on the cluster member.
	for peer, parent := range selected {
		system := systems[peer]
		if !bootstrap {
			if system.JoinConfig == nil {
				system.JoinConfig = []api.ClusterMemberConfigKey{}
			}

			system.JoinConfig = append(system.JoinConfig, lxd.DefaultOVNNetworkJoinConfig(parent))
		} else {
			system.TargetNetworks = append(system.TargetNetworks, lxd.DefaultPendingOVNNetwork(parent))
		}

		systems[peer] = system
	}

	if bootstrap {
		bootstrapSystem := systems[sh.Name]

		var ipv4Gateway string
		var ipv4Ranges string
		var ipv6Gateway string
		for gateway, ipRange := range ipConfig {
			ip, _, err := net.ParseCIDR(gateway)
			if err != nil {
				return err
			}

			if ip.To4() != nil {
				ipv4Gateway = gateway
				ipv4Ranges = ipRange
			} else {
				ipv6Gateway = gateway
			}
		}

		uplink, ovn := lxd.DefaultOVNNetwork(ipv4Gateway, ipv4Ranges, ipv6Gateway, dnsAddresses)
		bootstrapSystem.Networks = []api.NetworksPost{uplink, ovn}
		systems[sh.Name] = bootstrapSystem
	}

	return nil
}

func (c *initConfig) askCephNetwork(sh *service.Handler) error {
	if c.autoSetup {
		return nil
	}

	availableCephNetworkInterfaces := map[string][]service.CephDedicatedInterface{}
	for name, state := range c.state {
		if len(state.AvailableCephInterfaces) == 0 {
			fmt.Printf("No network interfaces found with IPs on %q to set a dedicated Ceph network, skipping Ceph network setup\n", name)

			return nil
		}

		ifaces := make([]service.CephDedicatedInterface, 0, len(state.AvailableCephInterfaces))
		for _, iface := range state.AvailableCephInterfaces {
			ifaces = append(ifaces, iface)
		}

		availableCephNetworkInterfaces[name] = ifaces
	}

	var defaultCephNetwork *net.IPNet
	for _, state := range c.state {
		if state.CephConfig != nil {
			value, ok := state.CephConfig["cluster_network"]
			if !ok || value == "" {
				continue
			}

			// Sometimes, the default cluster_network value in the Ceph configuration
			// is not a network range but a regular IP address. We need to extract the network range.
			_, valueNet, err := net.ParseCIDR(value)
			if err != nil {
				return fmt.Errorf("failed to parse the Ceph cluster network configuration from the existing Ceph cluster: %v", err)
			}

			defaultCephNetwork = valueNet
			break
		}
	}

	lxd := sh.Services[types.LXD].(*service.LXDService)
	if defaultCephNetwork != nil {
		if defaultCephNetwork.String() != "" && defaultCephNetwork.String() != c.lookupSubnet.String() {
			err := validateCephInterfacesForSubnet(lxd, c.systems, availableCephNetworkInterfaces, defaultCephNetwork.String())
			if err != nil {
				return err
			}
		}

		return nil
	}

	// MicroCeph is uninitialized, so ask the user for the network configuration.
	microCloudInternalNetworkAddr := c.lookupSubnet.IP.Mask(c.lookupSubnet.Mask)
	ones, _ := c.lookupSubnet.Mask.Size()
	microCloudInternalNetworkAddrCIDR := fmt.Sprintf("%s/%d", microCloudInternalNetworkAddr.String(), ones)
	internalCephSubnet, err := c.asker.AskString(fmt.Sprintf("What subnet (either IPv4 or IPv6 CIDR notation) would you like your Ceph internal traffic on? [default: %s] ", microCloudInternalNetworkAddrCIDR), microCloudInternalNetworkAddrCIDR, validate.IsNetwork)
	if err != nil {
		return err
	}

	if internalCephSubnet != microCloudInternalNetworkAddrCIDR {
		err = validateCephInterfacesForSubnet(lxd, c.systems, availableCephNetworkInterfaces, internalCephSubnet)
		if err != nil {
			return err
		}

		bootstrapSystem := c.systems[sh.Name]
		bootstrapSystem.MicroCephInternalNetworkSubnet = internalCephSubnet
		c.systems[sh.Name] = bootstrapSystem
	}

	return nil
}

// askClustered checks whether any of the selected systems have already initialized any expected services.
// If a service is already initialized on some systems, we will offer to add the remaining systems, or skip that service.
// In auto setup, we will expect no initialized services so that we can be opinionated about how we configure the cluster without user input.
// This works by deleting the record for the service from the `service.Handler`, thus ignoring it for the remainder of the setup.
func (c *initConfig) askClustered(s *service.Handler) error {
	expectedServices := make(map[types.ServiceType]struct{}, len(s.Services))
	for k := range s.Services {
		expectedServices[k] = struct{}{}
	}

	for serviceType := range expectedServices {
		for name, info := range c.state {
			_, newSystem := c.systems[name]
			if !newSystem {
				continue
			}

			if info.ServiceClustered(serviceType) {
				if c.autoSetup {
					return fmt.Errorf("%s is already clustered on %q, aborting setup", serviceType, info.ClusterName)
				}

				question := fmt.Sprintf("%q is already part of a %s cluster. Do you want to add this cluster to Microcloud? (add/skip) [default=add]", info.ClusterName, serviceType)
				validator := func(s string) error {
					if !shared.ValueInSlice(s, []string{"add", "skip"}) {
						return fmt.Errorf("Invalid input, expected one of (add,skip) but got %q", s)
					}

					return nil
				}

				addOrSkip, err := c.asker.AskString(question, "add", validator)
				if err != nil {
					return err
				}

				if addOrSkip != "add" {
					delete(s.Services, serviceType)
				}

				break
			}
		}
	}

	return nil
}
