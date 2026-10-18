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
./lebgazette discover -data data               # years, issues (295 issues for 2022-2026); -refresh finds new ones
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
to start until that file is gone (the daily crawl removes it on a later day). There are no proxies and no retries
after a block. The pages carry a passive fingerprinting script; it is not
needed and not imitated.

## Daily crawl

PCM answers with a CAPTCHA after a few hundred fetches in a short time (79 at
1.5 s apart, 570 at about 12 s apart). `deploy/pcm-daily.sh DATA_DIR` takes a
small share each day instead: `discover -refresh` to find new issues, then
`images` for each line of `deploy/pcm-phases.txt` in order, all under one daily
budget of fetches (400 to start, 15 s apart). A challenge stops the day and sets
the next budget to 70% of what got through; a clean day adds 25, up to 450. The
stop file is cleared only on a later day, never by solving the CAPTCHA. State
lives in the data directory: `pcm-budget`, `pcm-daily.log`, `pcm-blocks.log`.
Set `PCM_NOTIFY` to a shell command to hear about challenges and errors.

```sh
cp deploy/pcm-daily.service deploy/pcm-daily.timer ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now pcm-daily.timer
```

Layout under `data/raw/`: `year/<listing id>.html`, `issue/<page id>.html`,
`text/<jYYYY>/<jN>/<folder>/<file>.htm`, `img/<jYYYY>/<jN>/<docNN>/<page>.gif`.

MIT. Taktek and Nizar Mahmoud.
