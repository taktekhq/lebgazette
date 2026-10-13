# lebgazette

Crawler for the free copy of Lebanon's Official Gazette (الجريدة الرسمية) on
the Presidency of the Council of Ministers site, pcm.gov.lb. It stores the
year listings, each issue's table of contents, each text's viewer page and the
typeset page images, and writes a tab-separated index.

The site has no legal effect (its own disclaimer). Certified copies come from
the Gazette office. Whatever you publish from this data should say so and link
the official page.

## Use

```sh
go build -o lebgazette .
./lebgazette discover -data data               # years, issues (295 issues for 2022-2026)
./lebgazette texts    -data data -years 2026   # issue pages + each text's viewer
./lebgazette images   -data data -years 2026   # the page images
./lebgazette index    -data data               # data/index/texts.tsv
./lebgazette status   -data data
```

Flags: `-gap` (default 1.5s between requests), `-ua`, `-part2` (also fetch
Part Two: court notices, trademarks), `-limit N` (stop after N network
fetches), `-maxspan` (skip a text whose page range looks like a typo).

## Manners

One request at a time, a gap between requests, an honest User-Agent, each file
fetched once and kept. A 403, 429, 503 or a page mentioning a CAPTCHA stops
the run and writes `data/BLOCKED` with the URL and time; the next run refuses
to start until you delete that file. There are no proxies and no retries
after a block. The pages carry a passive fingerprinting script; it is not
needed and not imitated.

Layout under `data/raw/`: `year/<listing id>.html`, `issue/<page id>.html`,
`text/<jYYYY>/<jN>/<folder>/<file>.htm`, `img/<jYYYY>/<jN>/<docNN>/<page>.gif`.

MIT. Taktek and Nizar Mahmoud.
