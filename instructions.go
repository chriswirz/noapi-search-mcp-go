package main

// Instructions is what a client shows a model about this server as a whole. It
// is deliberately about choosing between the tools rather than describing each
// one, because the per-tool descriptions already say what each does and a model
// that picks the wrong tool wastes a turn no description can win back.
const Instructions = `Real search results and web content, with no API keys and no
usage limits: every tool here drives a headless browser or reads a search
engine's plain HTML endpoint directly.

Choosing a tool:
  - web_search is the general one, and takes an engine: google (default),
    duckduckgo, bing, brave, startpage or mojeek. Reach for duckduckgo when
    google is rate-limited, and for a second engine when a result looks wrong -
    the engines disagree, and that disagreement is information.
  - google_news, google_scholar, google_books, google_images and
    google_shopping are the vertical searches. They are Google-only.
  - visit_page reads one URL. Use it after a search: search results carry a
    snippet, not the page, and answering from a snippet alone is how a wrong
    answer gets stated confidently.
  - geocode turns an address into latitude and longitude. google_maps searches
    for places and returns coordinates for each; google_maps_directions routes
    between two of them.
  - google_weather, google_finance, google_trends, google_translate,
    google_flights and google_hotels read Google's answer cards.

Two things worth knowing. Google rate-limits scrapers: a tool that reports being
blocked is not broken, and the fix is to wait or switch engine, not to retry
immediately. And scraped results are shaped by whatever the page looked like at
the time, so a field that comes back empty means it was not on the page, not
that it does not exist.`
