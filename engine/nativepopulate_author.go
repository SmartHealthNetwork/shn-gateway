package engine

import "github.com/SmartHealthNetwork/shn-gateway/internal/cqfattribution"

func identifyCQLSoftwareAuthor(raw []byte) ([]byte, error) { return cqfattribution.Normalize(raw) }
