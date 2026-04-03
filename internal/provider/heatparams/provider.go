package heatparams

import "github.com/ventus-ag/magnum-bootstrap/internal/config"

type Provider struct {
	Path string
}

func (p Provider) Provide() (config.Config, error) {
	return config.Load(p.Path)
}
