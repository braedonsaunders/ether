package app

import "github.com/braedonsaunders/ether/internal/simkl"

func (a *App) simklAuth() simkl.AuthClient {
	c := a.cfg.Get().Simkl
	return simkl.AuthClient{ClientID: c.ClientID, ClientSecret: c.ClientSecret}
}
func (a *App) simklService() simkl.Service {
	return simkl.Service{Auth: a.simklAuth(), Tokens: simkl.TokenStore{DB: a.db, Sealer: a.cfg}, State: a.state}
}
