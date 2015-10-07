package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/packethost/packngo"
)

func resourcePacketDevice() *schema.Resource {
	return &schema.Resource{
		Create: resourcePacketDeviceCreate,
		Read:   resourcePacketDeviceRead,
		Update: resourcePacketDeviceUpdate,
		Delete: resourcePacketDeviceDelete,

		Schema: map[string]*schema.Schema{
			"os": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"hostname": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
			},

			"facility": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"plan": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
			},

			"project_id": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
				// DefaultFunc: schema.EnvDefaultFunc("PACKET_PROJECT_ID", nil),
			},

			"state": &schema.Schema{
				Type:     schema.TypeString,
				Computed: true,
			},

			"locked": &schema.Schema{
				Type:     schema.TypeBool,
				Computed: true,
			},

			"ipv6_address": &schema.Schema{
				Type:     schema.TypeString,
				Computed: true,
			},

			"ipv4_address": &schema.Schema{
				Type:     schema.TypeString,
				Computed: true,
			},

			"ipv4_address_private": &schema.Schema{
				Type:     schema.TypeString,
				Computed: true,
			},

			"tags": &schema.Schema{
				Type:     schema.TypeList,
				Optional: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
			},

			"user_data": &schema.Schema{
				Type:     schema.TypeString,
				Optional: true,
			},
		},
	}
}

func resourcePacketDeviceCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*packngo.Client)

	// Build up our creation options
	opts := &packngo.DeviceCreateRequest{
		OS:           d.Get("os").(string),
		HostName:     d.Get("hostname").(string),
		Facility:     d.Get("facility").(string),
		Plan:         d.Get("plan").(string),
		ProjectID:    d.Get("project_id").(string),
		BillingCycle: "hourly",
	}

	if attr, ok := d.GetOk("user_data"); ok {
		opts.UserData = attr.(string)
	}

	// Get configured tags
	tags := d.Get("tags.#").(int)
	if tags > 0 {
		opts.Tags = make([]string, 0, tags)
		for i := 0; i < tags; i++ {
			key := fmt.Sprintf("tags.%d", i)
			opts.Tags = append(opts.Tags, d.Get(key).(string))
		}
	}

	log.Printf("[DEBUG] Device create configuration: %#v", opts)

	dev, _, err := client.Devices.Create(opts)

	if err != nil {
		return fmt.Errorf("Error creating device: %s", err)
	}

	// Assign the device's id
	d.SetId(dev.ID)

	log.Printf("[INFO] Device ID: %s", d.Id())

	_, err = WaitForDeviceAttribute(d, "active", []string{"queued", "provisioning"}, "state", meta)
	if err != nil {
		return fmt.Errorf("Error waiting for device (%s) to become ready: %s", d.Id(), err)
	}

	return resourcePacketDeviceRead(d, meta)
}

func resourcePacketDeviceRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*packngo.Client)

	// Retrieve the device properties for updating the state
	dev, _, err := client.Devices.Get(d.Id())
	if err != nil {
		// check if the device no longer exists.
		// TODO: This is all wrong for Packet.
		if strings.Contains(err.Error(), "404 Not Found") {
			d.SetId("")
			return nil
		}

		return fmt.Errorf("Error retrieving device: %s", err)
	}

	d.Set("os", dev.OS.Slug)
	d.Set("hostname", dev.Hostname)
	d.Set("facility", dev.Facility.Code)
	d.Set("plan", dev.Plan.Slug)
	d.Set("state", dev.State)
	d.Set("locked", dev.Locked)

	var publicIPv4 string
	for _, addr := range dev.Network {
		switch addr.Family {
		case 4:
			if addr.Public {
				publicIPv4 = addr.Address
				d.Set("ipv4_address", addr.Address)
			} else {
				d.Set("ipv4_address_private", addr.Address)
			}
		case 6:
			if addr.Public {
				d.Set("ipv6_address", addr.Address)
			}
		}
	}

	// Initialize the connection info
	d.SetConnInfo(map[string]string{
		"type": "ssh",
		"host": publicIPv4,
	})

	return nil
}

func resourcePacketDeviceUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*packngo.Client)

	// TODO: Support changing hostname.

	if d.HasChange("locked") {
		var (
			action string
			err    error
		)
		if d.Get("locked").(bool) {
			action = "locking"
			_, err = client.Devices.Lock(d.Id())
		} else {
			action = "unlocking"
			_, err = client.Devices.Unlock(d.Id())
		}
		if err != nil {
			return fmt.Errorf("Error %s device (%s): %s", action, d.Id(), err)
		}
	}

	return resourcePacketDeviceRead(d, meta)
}

func resourcePacketDeviceDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*packngo.Client)

	_, err := WaitForDeviceAttribute(d, "active", []string{"queued", "provisioning"}, "state", meta)
	if err != nil {
		return fmt.Errorf("Error waiting for device to be active for destroy (%s): %s", d.Id(), err)
	}

	log.Printf("[INFO] Deleting device: %s", d.Id())

	// Destroy the device
	_, err = client.Devices.Delete(d.Id())

	// Handle remotely destroyed devices
	if err != nil && strings.Contains(err.Error(), "404 Not Found") {
		return nil
	}

	if err != nil {
		return fmt.Errorf("Error deleting device: %s", err)
	}

	return nil
}

func WaitForDeviceAttribute(d *schema.ResourceData, target string, pending []string, attribute string, meta interface{}) (interface{}, error) {
	// Wait for the device so we can get the networking attributes
	// that show up after a while
	log.Printf(
		"[INFO] Waiting for device (%s) to have %s of %s",
		d.Id(), attribute, target)

	stateConf := &resource.StateChangeConf{
		Pending:    pending,
		Target:     target,
		Refresh:    newDeviceStateRefreshFunc(d, attribute, meta),
		Timeout:    60 * time.Minute,
		Delay:      10 * time.Second,
		MinTimeout: 3 * time.Second,
	}

	return stateConf.WaitForState()
}

// TODO This function still needs a little more refactoring to make it
// cleaner and more efficient
func newDeviceStateRefreshFunc(d *schema.ResourceData, attribute string, meta interface{}) resource.StateRefreshFunc {
	client := meta.(*packngo.Client)
	return func() (interface{}, string, error) {
		err := resourcePacketDeviceRead(d, meta)
		if err != nil {
			return nil, "", err
		}

		// If the device is locked, continue waiting. We can
		// only perform actions on unlocked devices, so it's
		// pointless to look at that status
		// if d.Get("locked").(bool) {
		// 	log.Println("[DEBUG] Device is locked, skipping status check and retrying")
		// 	return nil, "", nil
		// }

		// See if we can access our attribute
		if attr, ok := d.GetOk(attribute); ok {
			// Retrieve the device properties
			dev, _, err := client.Devices.Get(d.Id())
			if err != nil {
				return nil, "", fmt.Errorf("Error retrieving device: %s", err)
			}

			return dev, attr.(string), nil
		}

		return nil, "", nil
	}
}

// Powers on the device and waits for it to be active
func powerOnAndWait(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*packngo.Client)
	_, err := client.Devices.PowerOn(d.Id())
	if err != nil {
		return err
	}

	// Wait for power on
	_, err = WaitForDeviceAttribute(d, "active", []string{"off"}, "state", client)
	if err != nil {
		return err
	}

	return nil
}
