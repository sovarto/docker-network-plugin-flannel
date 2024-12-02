package docker

import (
	"context"
	"github.com/docker/docker/api/types/events"
	"log"
	"time"
)

func (d *data) handleDockerEvents() {
	eventsCh, errCh := d.dockerClient.Events(context.Background(), events.ListOptions{})
	for {
		select {
		case err := <-errCh:
			log.Printf("Unable to connect to docker events channel, reconnecting..., err: %+v\n", err)
			time.Sleep(5 * time.Second)
			eventsCh, errCh = d.dockerClient.Events(context.Background(), events.ListOptions{})
		case event := <-eventsCh:
			err := d.handleEvent(event)
			if err != nil {
				log.Printf("Error handling docker event %+v, error: %+v\n", event, err)
			}
		}
	}
}

func (d *data) handleEvent(event events.Message) error {
	d.Lock()
	defer d.Unlock()

	switch event.Type {
	case events.NetworkEventType:
		switch event.Action {
		case events.ActionUpdate:
			return d.handleNetwork(event.Actor.ID)
		case events.ActionDelete:
			return d.handleDeletedNetwork(event.Actor.ID)
		case events.ActionConnect:
			return d.handleContainer(event.Actor.Attributes["container"])
		case events.ActionDisconnect:
			return d.handleDeletedContainer(event.Actor.Attributes["container"])
		}
	case events.ContainerEventType:
		switch event.Action {
		case events.ActionKill:
		}
	case events.ServiceEventType:
		switch event.Action {
		case events.ActionCreate:
			return d.handleService(event.Actor.ID)
		case events.ActionUpdate:
			return d.handleService(event.Actor.ID)
		case events.ActionDelete:
			return d.handleDeletedService(event.Actor.ID)
		}
	}

	return nil
}
