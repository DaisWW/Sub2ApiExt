package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

func rateChangeSignificant(current, target float64) bool {
	if almostEqual(current, target) {
		return false
	}
	absChange := math.Abs(target - current)
	relativeChange := absChange / math.Max(math.Abs(current), 0.0001)
	return absChange >= 0.005 || relativeChange >= 0.01
}

func channelStateKey(channel *Channel) string {
	return fmt.Sprintf("account:%d/group:%d", channel.AccountID, channel.Group.ID)
}

func channelStateKeyForTarget(channel *Channel, target string) string {
	if target == "account" {
		return fmt.Sprintf("account:%d", channel.AccountID)
	}
	return channelStateKey(channel)
}

func channelIdentity(channel *Channel) string {
	return channelIdentityForTarget(channel, "group")
}

func channelIdentityForTarget(channel *Channel, target string) string {
	digest := sha256.Sum256([]byte(channel.APIKey))
	identity := fmt.Sprintf(
		"%d|%s|%s",
		channel.AccountID,
		strings.TrimRight(strings.ToLower(channel.BaseURL), "/"),
		hex.EncodeToString(digest[:8]),
	)
	if target != "account" {
		identity = fmt.Sprintf("%s|%d", identity, channel.Group.ID)
	}
	if proxyURL := strings.TrimSpace(channel.ProxyURL); proxyURL != "" {
		proxyDigest := sha256.Sum256([]byte(proxyURL))
		identity += "|" + hex.EncodeToString(proxyDigest[:4])
	}
	return identity
}

func channelLabel(channel *Channel) string {
	account := strings.TrimSpace(channel.AccountName)
	group := strings.TrimSpace(channel.Group.Name)
	if account == group || account == "" {
		return group
	}
	return account + " → " + group
}

func round4(value float64) float64 {
	return math.Round(value*10000) / 10000
}

func almostEqual(left, right float64) bool {
	return math.Abs(left-right) < 0.00005
}
