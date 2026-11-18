package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/flemzord/t3fleet/pkg/provider"
)

const MaxBatchCount = 100
const MaxConcurrency = 10

type BatchRequest struct {
	ID string `json:"id"`
	CreateRequest
	Count int `json:"count"`
}

type Batch struct {
	ID       string    `json:"id"`
	Machines []Machine `json:"machines"`
}

type batchRecord struct {
	Request BatchRequest `json:"request"`
	IDs     []string     `json:"ids"`
	Names   []string     `json:"names,omitempty"`
}

// PrepareBatch atomically reserves every local name and operation identity before
// any cloud allocation. Replaying an identical ID returns the original plan.
func (m *Manager) PrepareBatch(ctx context.Context, req BatchRequest) (Batch, error) {
	if err := ctx.Err(); err != nil {
		return Batch{}, err
	}
	if len(req.ID) != 32 {
		return Batch{}, ErrInvalid
	}
	if _, err := hex.DecodeString(req.ID); err != nil {
		return Batch{}, ErrInvalid
	}
	if req.Count < 1 || req.Count > MaxBatchCount || m.opts.Providers[req.Provider] == nil {
		return Batch{}, ErrInvalid
	}
	if req.Template == (provider.Template{}) {
		req.Template = m.opts.Templates[req.Provider]
	}
	if req.Template == (provider.Template{}) {
		return Batch{}, ErrInvalid
	}
	if req.Template.Source != "" && req.Template.Snapshot == "" {
		return Batch{}, fmt.Errorf("%w: batch creation requires an explicit snapshot for a source machine", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Batch{}, ErrClosed
	}
	if old, ok := m.batches[req.ID]; ok {
		comparison := old.Request
		comparison.Name = managedName(comparison.Name)
		candidate := req
		candidate.Name = managedName(candidate.Name)
		if comparison != candidate {
			return Batch{}, ErrConflict
		}
		return m.batchLocked(req.ID), nil
	}
	if !validName.MatchString(req.Name) {
		return Batch{}, ErrInvalid
	}
	names := make([]string, req.Count)
	for i := range names {
		names[i] = fmt.Sprintf("%s-%02d", managedName(req.Name), i+1)
		if !validName.MatchString(names[i]) {
			return Batch{}, fmt.Errorf("%w: name including t3fleet- and batch suffix must fit 32 characters", ErrInvalid)
		}
	}
	for _, h := range m.machines {
		for _, name := range names {
			if h.record.Name == name {
				return Batch{}, ErrConflict
			}
		}
	}
	b := batchRecord{Request: req, Names: names}
	rollback := func() {
		for _, id := range b.IDs {
			_ = m.machines[id].server.Close()
			delete(m.machines, id)
		}
		delete(m.batches, req.ID)
	}
	for _, name := range names {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			rollback()
			return Batch{}, err
		}
		id := hex.EncodeToString(raw[:])
		h, err := m.listen(record{Machine: Machine{ID: id, Name: name, Provider: req.Provider, State: "planned"}, Template: req.Template, Size: req.Size})
		if err != nil {
			rollback()
			return Batch{}, err
		}
		m.machines[id] = h
		b.IDs = append(b.IDs, id)
	}
	m.batches[req.ID] = b
	if err := m.persistLocked(); err != nil {
		rollback()
		return Batch{}, err
	}
	return m.batchLocked(req.ID), nil
}

func (m *Manager) Batch(id string) (Batch, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.batches[id]; !ok {
		return Batch{}, ErrNotFound
	}
	return m.batchLocked(id), nil
}

func (m *Manager) batchLocked(id string) Batch {
	b := m.batches[id]
	result := Batch{ID: id, Machines: make([]Machine, 0, len(b.IDs))}
	for i, machineID := range b.IDs {
		if h := m.machines[machineID]; h != nil {
			result.Machines = append(result.Machines, h.record.Machine)
		} else {
			name := fmt.Sprintf("%s-%02d", b.Request.Name, i+1)
			if len(b.Names) == len(b.IDs) {
				name = b.Names[i]
			}
			result.Machines = append(result.Machines, Machine{ID: machineID, Name: name, Provider: b.Request.Provider, State: "deleted"})
		}
	}
	return result
}
