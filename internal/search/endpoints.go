package search

import (
	"github.com/JeremiahM37/librarr/internal/config"
	"github.com/JeremiahM37/librarr/internal/sources"
)

// Endpoints implementations for GET /api/sources. Each reads the same resolved
// config field its Search path uses (the registry after ApplyEnvOverrides, plus
// the Anna's domain that config.Load and the settings UI keep in
// cfg.AnnasArchiveDomain), so what the API reports cannot drift from what the
// source actually contacts. All of them go through sources.HostList, which keeps
// hostnames only.
//
// Deliberately NOT reported:
//   - AudioBookBay trackers: tracker announce URLs commonly carry a per-user
//     passkey in the path or query.
//   - Prowlarr, Flibusta and BookTracker: those are operator-hosted service
//     URLs (frequently internal docker hostnames) set only through env/settings,
//     not part of the resolved sources registry, and BookTracker sits next to a
//     username and password. Add them only if the use case shows up.
//   - Z-Library's personal domain: it is learned at login and is tied to the
//     account.

// registryOf returns the resolved registry, or nil when a Config was built
// without one (several tests do).
func registryOf(cfg *config.Config) *sources.Registry {
	if cfg == nil {
		return nil
	}
	return cfg.Sources
}

// Endpoints returns the Anna's Archive domain in use.
func (a *AnnasArchive) Endpoints() []string {
	return sources.HostList(a.cfg.AnnasArchiveDomain)
}

// Endpoints returns the Anna's Archive domain in use; the manga driver shares it.
func (a *AnnasManga) Endpoints() []string { return a.annas.Endpoints() }

// Endpoints returns the AudioBookBay mirrors in failover order (the order
// Search tries them in).
func (a *AudioBookBay) Endpoints() []string { return sources.HostList(a.domains()...) }

// Endpoints returns the ThePirateBay API host.
func (t *ThePirateBay) Endpoints() []string {
	if registryOf(t.cfg) == nil {
		return nil
	}
	return sources.HostList(t.apiBase())
}

// Endpoints returns the Z-Library API host: ZLIBRARY_URL when set, else the
// registry default, exactly as apiBase resolves it.
func (z *ZLibrary) Endpoints() []string {
	if z.cfg.ZLibraryURL == "" && registryOf(z.cfg) == nil {
		return nil
	}
	return sources.HostList(z.apiBase())
}

// Endpoints returns the Project Gutenberg host.
func (g *Gutenberg) Endpoints() []string {
	if r := registryOf(g.cfg); r != nil {
		return sources.HostList(r.Gutenberg.URL)
	}
	return nil
}

// Endpoints returns the Librivox host.
func (l *Librivox) Endpoints() []string {
	if r := registryOf(l.cfg); r != nil {
		return sources.HostList(r.Librivox.URL)
	}
	return nil
}

// Endpoints returns the Standard Ebooks host.
func (s *StandardEbooks) Endpoints() []string {
	if r := registryOf(s.cfg); r != nil {
		return sources.HostList(r.StandardEbooks.URL)
	}
	return nil
}

// Endpoints returns the Open Library search and cover hosts.
func (o *OpenLibrary) Endpoints() []string {
	if r := registryOf(o.cfg); r != nil {
		return sources.HostList(r.OpenLibrary.SearchURL, r.OpenLibrary.CoverURL)
	}
	return nil
}

// Endpoints returns the MangaDex API, uploads and web hosts.
func (m *MangaDex) Endpoints() []string {
	if r := registryOf(m.cfg); r != nil {
		return sources.HostList(r.MangaDex.APIURL, r.MangaDex.UploadsURL, r.MangaDex.WebURL)
	}
	return nil
}

// Endpoints returns the Nyaa host.
func (n *NyaaManga) Endpoints() []string {
	if r := registryOf(n.cfg); r != nil {
		return sources.HostList(r.Nyaa.URL)
	}
	return nil
}

// Endpoints returns the host of every web-novel site in the registry, search
// URL first and then its base URL, in registry order.
func (w *WebNovel) Endpoints() []string {
	r := registryOf(w.cfg)
	if r == nil {
		return nil
	}
	values := make([]string, 0, 2*len(r.WebNovels))
	for _, site := range r.WebNovels {
		values = append(values, site.URL, site.BaseURL)
	}
	return sources.HostList(values...)
}
