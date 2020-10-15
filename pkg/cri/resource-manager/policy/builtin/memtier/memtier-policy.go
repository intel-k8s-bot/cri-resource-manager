// Copyright 2019 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package memtier

import (
	v1 "k8s.io/api/core/v1"
	resapi "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"

	"github.com/intel/cri-resource-manager/pkg/config"
	"github.com/intel/cri-resource-manager/pkg/cpuallocator"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/cache"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/events"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/introspect"
	"github.com/intel/cri-resource-manager/pkg/cri/resource-manager/kubernetes"

	policyapi "github.com/intel/cri-resource-manager/pkg/cri/resource-manager/policy"
	system "github.com/intel/cri-resource-manager/pkg/sysfs"
)

const (
	// PolicyName is the symbol used to pull us in as a builtin policy.
	PolicyName = "memtier"
	// PolicyDescription is a short description of this policy.
	PolicyDescription = "A policy for prototyping memory tiering."
	// PolicyPath is the path of this policy in the configuration hierarchy.
	PolicyPath = "policy." + PolicyName

	// ColdStartDone is the event generated for the end of a container cold start period.
	ColdStartDone = "cold-start-done"
	// DirtyBitReset is the event generated for the reseting of the soft-dirty bits for all processes in the containers.
	DirtyBitReset = "dirty-bit-reset"
)

// allocations is our cache.Cachable for saving resource allocations in the cache.
type allocations struct {
	policy *policy
	grants map[string]Grant
}

// policy is our runtime state for the memtier policy.
type policy struct {
	options        policyapi.BackendOptions  // options we were created or reconfigured with
	cache          cache.Cache               // pod/container cache
	sys            system.System             // system/HW topology info
	allowed        cpuset.CPUSet             // bounding set of CPUs we're allowed to use
	reserved       cpuset.CPUSet             // system-/kube-reserved CPUs
	reserveCnt     int                       // number of CPUs to reserve if given as resource.Quantity
	isolated       cpuset.CPUSet             // (our allowed set of) isolated CPUs
	nodes          map[string]Node           // pool nodes by name
	pools          []Node                    // pre-populated node slice for scoring, etc...
	root           Node                      // root of our pool/partition tree
	nodeCnt        int                       // number of pools
	depth          int                       // tree depth
	allocations    allocations               // container pool assignments
	cpuAllocator   cpuallocator.CPUAllocator // CPU allocator used by the policy
	dynamicDemoter Demoter                   // Dynamic demoter for moving memory pages
	coldstartOff   bool                      // coldstart forced off (have movable PMEM zones)
}

// Make sure policy implements the policy.Backend interface.
var _ policyapi.Backend = &policy{}

// Whether we have coldstart forced off due to PMEM in movable memory zones.
var coldStartOff bool

// CreateMemtierPolicy creates a new policy instance.
func CreateMemtierPolicy(opts *policyapi.BackendOptions) policyapi.Backend {
	p := &policy{
		cache:        opts.Cache,
		sys:          opts.System,
		options:      *opts,
		cpuAllocator: cpuallocator.NewCPUAllocator(opts.System),
	}

	p.nodes = make(map[string]Node)
	p.allocations = allocations{policy: p, grants: make(map[string]Grant, 32)}

	if err := p.checkConstraints(); err != nil {
		log.Fatal("failed to create memtier policy: %v", err)
	}

	if err := p.buildPoolsByTopology(); err != nil {
		log.Fatal("failed to create memtier policy: %v", err)
	}

	p.addImplicitAffinities()

	config.GetModule(PolicyPath).AddNotify(p.configNotify)

	p.dynamicDemoter = NewDemoter(p, &linuxPageMover{})

	p.root.Dump("<pre-start>")

	return p
}

// Name returns the name of this policy.
func (p *policy) Name() string {
	return PolicyName
}

// Description returns the description for this policy.
func (p *policy) Description() string {
	return PolicyDescription
}

// Start prepares this policy for accepting allocation/release requests.
func (p *policy) Start(add []cache.Container, del []cache.Container) error {
	if err := p.restoreCache(); err != nil {
		return policyError("failed to start: %v", err)
	}

	// Turn coldstart forcibly off if we have movable non-DRAM memory.
	// Note that although this can change dynamically we only check it
	// during startup and trust users to either not fiddle with memory
	// or restart us if they do.
	p.checkColdstartOff()

	// TODO: the dirty bit reset timer should only be started if there is a container
	// for which there is a demotion possiblity.
	p.dynamicDemoter.Reconfigure(opt.DirtyBitScanPeriod, opt.PageMovePeriod, opt.PageMoveCount)

	p.root.Dump("<post-start>")

	return p.Sync(add, del)
}

// Sync synchronizes the state of this policy.
func (p *policy) Sync(add []cache.Container, del []cache.Container) error {
	log.Debug("synchronizing state...")
	for _, c := range del {
		p.ReleaseResources(c)
	}
	for _, c := range add {
		p.AllocateResources(c)
	}

	return nil
}

// AllocateResources is a resource allocation request for this policy.
func (p *policy) AllocateResources(container cache.Container) error {
	log.Debug("allocating resources for %s...", container.PrettyName())

	grant, err := p.allocatePool(container)
	if err != nil {
		return policyError("failed to allocate resources for %s: %v",
			container.PrettyName(), err)
	}

	if err := p.applyGrant(grant); err != nil {
		if _, _, err = p.releasePool(container); err != nil {
			log.Warn("failed to undo/release unapplicable grant %s: %v", grant, err)
			return policyError("failed to undo/release unapplicable grant %s: %v", grant, err)
		}
	}

	if err := p.updateSharedAllocations(grant); err != nil {
		log.Warn("failed to update shared allocations affected by %s: %v",
			container.PrettyName(), err)
	}

	p.root.Dump("<post-alloc>")

	return nil
}

// ReleaseResources is a resource release request for this policy.
func (p *policy) ReleaseResources(container cache.Container) error {
	log.Debug("releasing resources of %s...", container.PrettyName())

	grant, found, err := p.releasePool(container)
	if err != nil {
		return policyError("failed to release resources of %s: %v",
			container.PrettyName(), err)
	}

	if found {
		if err = p.updateSharedAllocations(grant); err != nil {
			log.Warn("failed to update shared allocations affected by %s: %v",
				container.PrettyName(), err)
		}
	}

	p.root.Dump("<post-release>")

	return nil
}

// UpdateResources is a resource allocation update request for this policy.
func (p *policy) UpdateResources(c cache.Container) error {
	log.Debug("(not) updating container %s...", c.PrettyName())
	return nil
}

// Rebalance tries to find an optimal allocation of resources for the current containers.
func (p *policy) Rebalance() (bool, error) {
	var errors error

	containers := p.cache.GetContainers()
	movable := []cache.Container{}

	for _, c := range containers {
		if c.GetQOSClass() != v1.PodQOSGuaranteed {
			p.ReleaseResources(c)
			movable = append(movable, c)
		}
	}

	for _, c := range movable {
		if err := p.AllocateResources(c); err != nil {
			if errors == nil {
				errors = err
			} else {
				errors = policyError("%v, %v", errors, err)
			}
		}
	}

	return true, errors
}

// HandleEvent handles policy-specific events.
func (p *policy) HandleEvent(e *events.Policy) (bool, error) {
	log.Debug("received policy event %s.%s with data %v...", e.Source, e.Type, e.Data)

	switch e.Type {
	case events.ContainerStarted:
		c, ok := e.Data.(cache.Container)
		if !ok {
			return false, policyError("%s event: expecting cache.Container Data, got %T",
				e.Type, e.Data)
		}
		log.Info("triggering coldstart period (if necessary) for %s", c.PrettyName())
		return false, p.triggerColdStart(c)
	case ColdStartDone:
		id, ok := e.Data.(string)
		if !ok {
			return false, policyError("%s event: expecting container ID Data, got %T",
				e.Type, e.Data)
		}
		c, ok := p.cache.LookupContainer(id)
		if !ok {
			// TODO: This is probably a race condition. Should we return nil error here?
			return false, policyError("%s event: failed to lookup container %s", id)
		}
		log.Info("finishing coldstart period for %s", c.PrettyName())
		return p.finishColdStart(c)
	case DirtyBitReset:
		for _, container := range p.cache.GetContainers() {
			if container.GetNamespace() == kubernetes.NamespaceSystem {
				// The system containers should not be moved.
				continue
			}
			grant, ok := p.allocations.grants[container.GetCacheID()]
			if !ok {
				log.Info("%s event: no grant found for container %s", e.Type, container.GetCacheID())
				continue
			}
			memType := grant.GetMemoryNode().GetMemoryType()
			if memType&memoryDRAM == 0 || memType&memoryPMEM == 0 {
				log.Info("%s event: not demoting pages, memory type %v for container %v", e.Type, memType, container.GetCacheID())
				// No demotion possibility.
				continue
			}
			pmemNodes := grant.GetMemoryNode().GetMemset(memoryPMEM)
			dramNodes := grant.GetMemoryNode().GetMemset(memoryDRAM)

			// Gather the known pages which need to be moved.
			pagePool, err := p.dynamicDemoter.GetPagesForContainer(container, dramNodes)
			if err != nil {
				log.Error("%s event: failed to get pages for container %v", e.Type, container.GetCacheID())
				continue
			}

			count := 0
			for _, pages := range pagePool.pages {
				count += len(pages)
			}
			log.Debug("%s event: %d pages for (maybe) demoting for %v", e.Type, count, container.GetCacheID())

			// Reset the dirty bit from all pages.
			p.dynamicDemoter.ResetDirtyBit(container)

			// Give the pages to the page moving goroutine. Copy the page pool so that there's no race.
			p.dynamicDemoter.UpdateDemoter(container.GetCacheID(), copyPagePool(pagePool), pmemNodes.Clone())
		}
		cids := p.dynamicDemoter.UnusedDemoters(p.cache.GetContainers())
		for _, cid := range cids {
			p.dynamicDemoter.StopDemoter(cid)
		}
		return false, nil
	}
	return false, nil
}

// Introspect provides data for external introspection.
func (p *policy) Introspect(state *introspect.State) {
	pools := make(map[string]*introspect.Pool, len(p.pools))
	for _, node := range p.nodes {
		cpus := node.GetSupply()
		pool := &introspect.Pool{
			Name:   node.Name(),
			CPUs:   cpus.SharableCPUs().Union(cpus.IsolatedCPUs()).String(),
			Memory: node.GetMemset(memoryAll).String(),
		}
		if parent := node.Parent(); !parent.IsNil() {
			pool.Parent = parent.Name()
		}
		if children := node.Children(); len(children) > 0 {
			pool.Children = make([]string, 0, len(children))
			for _, c := range children {
				pool.Children = append(pool.Children, c.Name())
			}
		}
		pools[pool.Name] = pool
	}
	state.Pools = pools

	assignments := make(map[string]*introspect.Assignment, len(p.allocations.grants))
	for _, g := range p.allocations.grants {
		a := &introspect.Assignment{
			ContainerID:   g.GetContainer().GetID(),
			CPUShare:      g.SharedPortion(),
			ExclusiveCPUs: g.ExclusiveCPUs().Union(g.IsolatedCPUs()).String(),
			Pool:          g.GetCPUNode().Name(),
		}
		if g.SharedPortion() > 0 || a.ExclusiveCPUs == "" {
			a.SharedCPUs = g.SharedCPUs().String()
		}
		assignments[a.ContainerID] = a
	}
	state.Assignments = assignments
}

// ExportResourceData provides resource data to export for the container.
func (p *policy) ExportResourceData(c cache.Container) map[string]string {
	grant, ok := p.allocations.grants[c.GetCacheID()]
	if !ok {
		return nil
	}

	data := map[string]string{}
	shared := grant.SharedCPUs().String()
	isolated := grant.ExclusiveCPUs().Intersection(grant.GetCPUNode().GetSupply().IsolatedCPUs())
	exclusive := grant.ExclusiveCPUs().Difference(isolated).String()

	if shared != "" {
		data[policyapi.ExportSharedCPUs] = shared
	}
	if isolated.String() != "" {
		data[policyapi.ExportIsolatedCPUs] = isolated.String()
	}
	if exclusive != "" {
		data[policyapi.ExportExclusiveCPUs] = exclusive
	}

	mems := grant.Memset()
	dram := system.NewIDSet()
	pmem := system.NewIDSet()
	hbm := system.NewIDSet()
	for _, id := range mems.SortedMembers() {
		node := p.sys.Node(id)
		switch node.GetMemoryType() {
		case system.MemoryTypeDRAM:
			dram.Add(id)
		case system.MemoryTypePMEM:
			pmem.Add(id)
			/*
				case system.MemoryTypeHBM:
					hbm.Add(id)
			*/
		}
	}
	data["ALL_MEMS"] = mems.String()
	if dram.Size() > 0 {
		data["DRAM_MEMS"] = dram.String()
	}
	if pmem.Size() > 0 {
		data["PMEM_MEMS"] = pmem.String()
	}
	if hbm.Size() > 0 {
		data["HBM_MEMS"] = hbm.String()
	}

	return data
}

func (p *policy) configNotify(event config.Event, source config.Source) error {
	log.Info("configuration %s:", event)
	log.Info("  - pin containers to CPUs: %v", opt.PinCPU)
	log.Info("  - pin containers to memory: %v", opt.PinMemory)
	log.Info("  - prefer isolated CPUs: %v", opt.PreferIsolated)
	log.Info("  - prefer shared CPUs: %v", opt.PreferShared)
	log.Info("  - page scan period: %s", opt.DirtyBitScanPeriod.String())
	log.Info("  - page move period: %s", opt.PageMovePeriod.String())
	log.Info("  - page move count: %d", opt.PageMoveCount)

	p.dynamicDemoter.Reconfigure(opt.DirtyBitScanPeriod, opt.PageMovePeriod, opt.PageMoveCount)

	// TODO: We probably should release and reallocate resources for all containers
	//   to honor the latest configuration. Depending on the changes that might be
	//   disruptive to some containers, so whether we do so or not should probably
	//   be part of the configuration as well.

	p.saveConfig()

	return nil
}

// Check the constraints passed to us.
func (p *policy) checkConstraints() error {
	if c, ok := p.options.Available[policyapi.DomainCPU]; ok {
		p.allowed = c.(cpuset.CPUSet)
	} else {
		// default to all online cpus
		p.allowed = p.sys.CPUSet().Difference(p.sys.Offlined())
	}

	p.isolated = p.sys.Isolated().Intersection(p.allowed)

	c, ok := p.options.Reserved[policyapi.DomainCPU]
	if !ok {
		return policyError("cannot start without CPU reservation")
	}

	switch c.(type) {
	case cpuset.CPUSet:
		p.reserved = c.(cpuset.CPUSet)
		// check that all reserved CPUs are in the allowed set
		if !p.reserved.Difference(p.allowed).IsEmpty() {
			return policyError("invalid reserved cpuset %s, some CPUs (%s) are not "+
				"part of the online allowed cpuset (%s)", p.reserved,
				p.reserved.Difference(p.allowed), p.allowed)
		}
		// check that none of the reserved CPUs are isolated
		if !p.reserved.Intersection(p.isolated).IsEmpty() {
			return policyError("invalid reserved cpuset %s, some CPUs (%s) are also isolated",
				p.reserved.Intersection(p.isolated))
		}

	case resapi.Quantity:
		qty := c.(resapi.Quantity)
		p.reserveCnt = (int(qty.MilliValue()) + 999) / 1000
	}

	return nil
}

func (p *policy) restoreCache() error {
	if !p.restoreConfig() {
		log.Warn("no saved configuration found in cache...")
		p.saveConfig()
	}

	if err := p.restoreAllocations(); err != nil {
		return policyError("failed to restore cached allocations: %v", err)
	}
	p.allocations.Dump(log.Info, "restored ")
	p.saveAllocations()

	return nil
}

func (p *policy) checkColdstartOff() {
	for _, id := range p.sys.NodeIDs() {
		node := p.sys.Node(id)
		if node.GetMemoryType() == system.MemoryTypePMEM {
			if !node.HasNormalMemory() {
				coldStartOff = true
				log.Error("coldstart forced off: NUMA node #%d does not have normal memory", id)
				return
			}
		}
	}
}

// Register us as a policy implementation.
func init() {
	policyapi.Register(PolicyName, PolicyDescription, CreateMemtierPolicy)
}
