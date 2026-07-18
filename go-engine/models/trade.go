package models

import "time"

type Trade struct {
	ID        uint   `gorm:"primaryKey"`
	Symbol    string `gorm:"index"`
	Price     float64
	Quantity  int
	Side      string // BUY/SELL
	Timestamp time.Time
}

type Order struct {
	ID      uint   `gorm:"primaryKey"`
	OrderID string `gorm:"uniqueIndex"`
	Symbol  string
	Status  string
}
