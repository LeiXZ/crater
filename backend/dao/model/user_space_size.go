package model

import (
	"time"
)

// UserSpaceSize stores the latest explicitly refreshed usage for a user directory.
type UserSpaceSize struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"index" json:"user_id"`
	User      User      `gorm:"foreignKey:UserID" json:"user"`
	Username  string    `gorm:"size:64;not null;uniqueIndex" json:"username"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
