// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package hcloud

import (
	"context"
	"fmt"
	"io/ioutil"
	"sort"
	"strings"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

type stepCreateServer struct {
	serverId int64
}

func (s *stepCreateServer) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	client := state.Get("hcloudClient").(*hcloud.Client)
	ui := state.Get("ui").(packersdk.Ui)
	c := state.Get("config").(*Config)
	sshKeyId := state.Get("ssh_key_id").(int64)

	// Create the server based on configuration
	ui.Say("Creating server...")

	userData := c.UserData
	if c.UserDataFile != "" {
		contents, err := ioutil.ReadFile(c.UserDataFile)
		if err != nil {
			state.Put("error", fmt.Errorf("Problem reading user data file: %s", err))
			return multistep.ActionHalt
		}

		userData = string(contents)
	}

	sshKeys := []*hcloud.SSHKey{{ID: sshKeyId}}
	for _, k := range c.SSHKeys {
		sshKey, _, err := client.SSHKey.Get(ctx, k)
		if err != nil {
			ui.Error(err.Error())
			state.Put("error", fmt.Errorf("Error fetching SSH key: %s", err))
			return multistep.ActionHalt
		}
		if sshKey == nil {
			state.Put("error", fmt.Errorf("Could not find key: %s", k))
			return multistep.ActionHalt
		}
		sshKeys = append(sshKeys, sshKey)
	}

	var image *hcloud.Image
	if c.Image != "" {
		image = &hcloud.Image{Name: c.Image}
	} else {
		serverType := state.Get("serverType").(*hcloud.ServerType)
		var err error
		image, err = getImageWithSelectors(ctx, client, c, serverType)
		if err != nil {
			ui.Error(err.Error())
			state.Put("error", err)
			return multistep.ActionHalt
		}
		ui.Message(fmt.Sprintf("Using image %s with ID %d", image.Description, image.ID))
	}

	var networks []*hcloud.Network
	for _, k := range c.Networks {
		networks = append(networks, &hcloud.Network{ID: k})
	}

	serverCreateOpts := hcloud.ServerCreateOpts{
		Name:       c.ServerName,
		ServerType: &hcloud.ServerType{Name: c.ServerType},
		Image:      image,
		SSHKeys:    sshKeys,
		Location:   &hcloud.Location{Name: c.Location},
		UserData:   userData,
		Networks:   networks,
		Labels:     c.ServerLabels,
	}

	if c.UpgradeServerType != "" {
		serverCreateOpts.StartAfterCreate = hcloud.Bool(false)
	}

	serverCreateResult, _, err := client.Server.Create(ctx, serverCreateOpts)
	if err != nil {
		err := fmt.Errorf("Error creating server: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	state.Put("server_ip", serverCreateResult.Server.PublicNet.IPv4.IP.String())
	// We use this in cleanup
	s.serverId = serverCreateResult.Server.ID

	// Store the server id for later
	state.Put("server_id", serverCreateResult.Server.ID)
	// instance_id is the generic term used so that users can have access to the
	// instance id inside of the provisioners, used in step_provision.
	state.Put("instance_id", serverCreateResult.Server.ID)

	if err := waitForAction(ctx, client, serverCreateResult.Action); err != nil {
		err := fmt.Errorf("Error creating server: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	for _, nextAction := range serverCreateResult.NextActions {
		if err := waitForAction(ctx, client, nextAction); err != nil {
			err := fmt.Errorf("Error creating server: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return multistep.ActionHalt
		}
	}

	if c.UpgradeServerType != "" {
		ui.Say("Changing server-type...")
		serverChangeTypeAction, _, err := client.Server.ChangeType(ctx, serverCreateResult.Server, hcloud.ServerChangeTypeOpts{
			ServerType:  &hcloud.ServerType{Name: c.UpgradeServerType},
			UpgradeDisk: false,
		})
		if err != nil {
			err := fmt.Errorf("Error changing server-type: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return multistep.ActionHalt
		}

		if err := waitForAction(ctx, client, serverChangeTypeAction); err != nil {
			err := fmt.Errorf("Error changing server-type: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return multistep.ActionHalt
		}

		ui.Say("Starting server...")
		serverPoweronAction, _, err := client.Server.Poweron(ctx, serverCreateResult.Server)
		if err != nil {
			err := fmt.Errorf("Error starting server: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return multistep.ActionHalt
		}

		if err := waitForAction(ctx, client, serverPoweronAction); err != nil {
			err := fmt.Errorf("Error starting server: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return multistep.ActionHalt
		}
	}

	if c.RescueMode != "" {
		ui.Say("Enabling Rescue Mode...")
		_, err := setRescue(ctx, client, serverCreateResult.Server, c.RescueMode, sshKeys)
		if err != nil {
			err := fmt.Errorf("Error enabling rescue mode: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return multistep.ActionHalt
		}
		ui.Say("Reboot server...")
		action, _, err := client.Server.Reset(ctx, serverCreateResult.Server)
		if err != nil {
			err := fmt.Errorf("Error rebooting server: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return multistep.ActionHalt
		}
		if err := waitForAction(ctx, client, action); err != nil {
			err := fmt.Errorf("Error rebooting server: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return multistep.ActionHalt
		}
	}

	return multistep.ActionContinue
}

func (s *stepCreateServer) Cleanup(state multistep.StateBag) {
	// If the serverID isn't there, we probably never created it
	if s.serverId == 0 {
		return
	}

	client := state.Get("hcloudClient").(*hcloud.Client)
	ui := state.Get("ui").(packersdk.Ui)

	// Destroy the server we just created
	ui.Say("Destroying server...")
	_, err := client.Server.Delete(context.TODO(), &hcloud.Server{ID: s.serverId})
	if err != nil {
		ui.Error(fmt.Sprintf(
			"Error destroying server. Please destroy it manually: %s", err))
	}
}

func setRescue(ctx context.Context, client *hcloud.Client, server *hcloud.Server, rescue string, sshKeys []*hcloud.SSHKey) (string, error) {
	rescueChanged := false
	if server.RescueEnabled {
		rescueChanged = true
		action, _, err := client.Server.DisableRescue(ctx, server)
		if err != nil {
			return "", err
		}
		if err := waitForAction(ctx, client, action); err != nil {
			return "", err
		}
	}

	if rescue != "" {
		res, _, err := client.Server.EnableRescue(ctx, server, hcloud.ServerEnableRescueOpts{
			Type:    hcloud.ServerRescueType(rescue),
			SSHKeys: sshKeys,
		})
		if err != nil {
			return "", err
		}
		if err := waitForAction(ctx, client, res.Action); err != nil {
			return "", err
		}
		return res.RootPassword, nil
	}

	if rescueChanged {
		action, _, err := client.Server.Reset(ctx, server)
		if err != nil {
			return "", err
		}
		if err := waitForAction(ctx, client, action); err != nil {
			return "", err
		}
	}
	return "", nil
}

func waitForAction(ctx context.Context, client *hcloud.Client, action *hcloud.Action) error {
	_, errCh := client.Action.WatchProgress(ctx, action)
	if err := <-errCh; err != nil {
		return err
	}
	return nil
}

func getImageWithSelectors(ctx context.Context, client *hcloud.Client, c *Config, serverType *hcloud.ServerType) (*hcloud.Image, error) {
	var allImages []*hcloud.Image

	selector := strings.Join(c.ImageFilter.WithSelector, ",")
	opts := hcloud.ImageListOpts{
		ListOpts:     hcloud.ListOpts{LabelSelector: selector},
		Status:       []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
		Architecture: []hcloud.Architecture{serverType.Architecture},
	}

	allImages, err := client.Image.AllWithOpts(ctx, opts)
	if err != nil {
		return nil, err
	}
	if len(allImages) == 0 {
		return nil, fmt.Errorf("no image found for selector %q", selector)
	}
	if len(allImages) > 1 {
		if !c.ImageFilter.MostRecent {
			return nil, fmt.Errorf("more than one image found for selector %q", selector)
		}

		sort.Slice(allImages, func(i, j int) bool {
			return allImages[i].Created.After(allImages[j].Created)
		})
	}

	return allImages[0], nil
}
