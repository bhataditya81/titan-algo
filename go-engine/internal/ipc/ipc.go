package ipc

import "log"

type Client struct {
	// TODO: Arrow Flight Client
}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) WriteBatch(data interface{}) error {
	log.Println("Writing batch to Shared Memory...")
	return nil
}
