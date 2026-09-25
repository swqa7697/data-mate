package cli

import "github.com/swqa7697/data-mate/internal/config"

func (b Build) resolveRoot(override, exe string) (config.Root, error) {
	switch b.Environment.Kind() {
	case config.Development:
		return config.ResolveRoot(override, exe)
	case config.Production:
		if override != "" {
			return config.Root{}, config.ErrOwnership
		}
		home := b.accountHome
		if home == nil {
			home = config.AccountHome
		}
		path, err := home()
		if err != nil {
			return config.Root{}, err
		}
		return config.ProductionRoot(path)
	default:
		return config.Root{}, config.ErrOwnership
	}
}
