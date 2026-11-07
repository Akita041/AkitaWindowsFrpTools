package tunnel

import "context"

type logger func(string)

func RunServer(ctx context.Context, c ServerConfig, log logger) error {
	switches := NewMappingSwitches()
	for _, m := range c.Mappings {
		switches.SetEnabled(ServerMappingSwitchKey(m), !m.Disabled)
	}
	return RunServerManaged(ctx, c, switches, log)
}

func RunClient(ctx context.Context, c ClientConfig, log logger) error {
	switches := NewMappingSwitches()
	for _, m := range c.Mappings {
		switches.SetEnabled(ClientMappingSwitchKey(m), !m.Disabled)
	}
	return RunClientManaged(ctx, c, switches, log)
}
