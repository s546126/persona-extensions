package main

import "testing"

func TestPickMachineSlotPrefersHealthyMachinesAndRespectsPins(t *testing.T) {
	machines := []MachineSlot{
		{Name: "machine-1", Title: "Primary", Online: true, Degraded: true, MaxSlots: 1},
		{Name: "machine-2", Online: true, MaxSlots: 2, RunningSlots: 1},
	}
	if idx, why := pickMachineSlot(machines, "", "", nil); idx != 1 || why != "" {
		t.Fatalf("healthy slot: index=%d reason=%q", idx, why)
	}
	if idx, why := pickMachineSlot(machines, " primary ", "", nil); idx != 0 || why != "" {
		t.Fatalf("pinned slot: index=%d reason=%q", idx, why)
	}
	if idx, why := pickMachineSlot(machines[:1], "", "", nil); idx != 0 || why != "" {
		t.Fatalf("only degraded slot: index=%d reason=%q", idx, why)
	}
	attempts := make([]Attempt, 4)
	for i := range attempts {
		attempts[i] = Attempt{Machine: "machine-2", State: StateFailed, ErrorClass: "gateway_error"}
	}
	if idx, why := pickMachineSlot(machines, "", "machine-2:worker-standard", attempts); idx != 1 || why != "" {
		t.Fatalf("failure history outweighed current health: index=%d reason=%q", idx, why)
	}
	machines[1].RunningSlots = machines[1].MaxSlots
	if idx, why := pickMachineSlot(machines, "", "", nil); idx != -1 || why != "no_free_machine_slots" {
		t.Fatalf("should wait for the healthy machine's slot: index=%d reason=%q", idx, why)
	}
}

func TestPickMachineSlotCountsEachGatewayFailure(t *testing.T) {
	machines := []MachineSlot{{Name: "machine-1", Online: true, MaxSlots: 1}, {Name: "machine-2", Online: true, MaxSlots: 1}}
	for _, class := range []string{"gateway_unreachable", "gateway_error"} {
		attempts := []Attempt{{Machine: "machine-1", State: StateFailed, ErrorClass: class}}
		for range 4 {
			attempts = append(attempts, Attempt{Machine: "machine-2", State: StateFailed})
		}
		attempts = append(attempts, Attempt{Machine: "machine-2", State: StateCanceled, ErrorClass: class})
		if idx, why := pickMachineSlot(machines, "", "machine-2:worker-standard", attempts); idx != 1 || why != "" {
			t.Errorf("gateway failure %q should outweigh four ordinary failures and last worker bias: index=%d reason=%q", class, idx, why)
		}
	}
}

func TestScheduleRemembersAllFailedMachines(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	if err := q.SetMachines([]MachineSlot{
		{Name: "machine-1", Online: true, MaxSlots: 1},
		{Name: "machine-4", Online: true, MaxSlots: 1},
		{Name: "machine-9", Online: true, MaxSlots: 1},
	}); err != nil {
		t.Fatal(err)
	}
	task := mustEnqueue(t, q, EnqueueInput{})
	now := testStart
	for _, want := range []string{"machine-1", "machine-4", "machine-9"} {
		_, attempts, _, err := q.DispatchNext(1, now)
		if err != nil || len(attempts) != 1 {
			t.Fatalf("dispatch: %v, %v", attempts, err)
		}
		att := attempts[0]
		if att.Machine != want {
			t.Fatalf("attempt %d ran on %s, want %s", att.AttemptNo, att.Machine, want)
		}
		if att.AttemptNo == 3 {
			break
		}
		got, err := q.CompleteAttempt(CompleteAttemptInput{
			TaskID: task.ID, FenceToken: att.FenceToken, WorkerID: att.WorkerID,
			Outcome: StateFailed, ErrorClass: "task_failed", Now: now,
		})
		if err != nil || got.NotBefore == nil {
			t.Fatalf("expected a queued retry, got %+v, %v", got, err)
		}
		now = *got.NotBefore
	}
}
