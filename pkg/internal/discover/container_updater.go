package discover

import (
	"github.com/mariomac/pipes/pipe"

	"github.com/grafana/beyla/pkg/internal/transform/kube"
)

type processInfoAccess interface {
	PID() uint32
	ProcessNS() uint32
}

// ContainerDBUpdaterProvider is a stage in the Process Finder pipeline that will be
// enabled only if Kubernetes decoration is enabled.
// It just updates part of the kubernetes database when a new process is discovered.
func ContainerDBUpdaterProvider[PI processInfoAccess](enabled bool, db *kube.Database) pipe.MiddleProvider[[]Event[PI], []Event[PI]] {
	return func() (pipe.MiddleFunc[[]Event[PI], []Event[PI]], error) {
		if !enabled {
			return pipe.Bypass[[]Event[PI]](), nil
		}
		return updateLoop[PI](db), nil
	}
}

func updateLoop[PI processInfoAccess](db *kube.Database) pipe.MiddleFunc[[]Event[PI], []Event[PI]] {
	return func(in <-chan []Event[PI], out chan<- []Event[PI]) {
		for instrumentables := range in {
			for i := range instrumentables {
				ev := &instrumentables[i]
				switch ev.Type {
				case EventCreated:
					db.AddProcess(ev.Obj.PID())
				case EventDeleted:
					// we don't need to handle process deletion from here, as the Kubernetes informer will
					// remove the process from the database when the Pod that contains it is deleted.
					// However we clean-up the performance related caches, in case we miss pod removal event
					db.CleanProcessCaches(ev.Obj.ProcessNS())
				}
			}
			out <- instrumentables
		}
	}
}
