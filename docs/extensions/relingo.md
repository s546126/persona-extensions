# Relingo

[Documentation](../README.md) · [Source](../../extensions/relingo/)

Relingo adds a reading page with two modes. Translation mode keeps every sentence and places a translation under it. Vocabulary mode (生词模式) leaves the same sentences in place and underlines candidate words. Choosing an underlined word shows its gloss and can remember the word. The app can also look a word up and save or remove it through the same list. Remembering the same word again does not create a second entry.

## Renamed from Termwise

Through version 0.1.2 this extension's id was `termwise`. Version 0.2.0 changes the id, directory and binaries to `relingo`. The source manifest declares `replaces: "termwise"`; the packaged runtime manifest does not. Once the new package and catalog are published, use the same registered market's normal preview and update flow on a hub that supports identity transitions. The confirmation names both ids. Settings and the data directory stay on the original storage slot, and `reading.json` is unchanged, so saved words carry over. Do not uninstall the former extension or install a local new-id archive to simulate this upgrade.

The name Relingo is also used by an existing commercial English-vocabulary product. This extension is not that product and is not affiliated with it. Maintainers should weigh that before publication.

## No command line needed

Relingo is a self-contained program: the dictionary, the candidate rules and the developer word list are built into it, and it starts no other program. Installing it from the market is all a user does; the reading page and the in-app lookup work without a terminal. Translation mode is the only part that reaches the network (`api.mymemory.translated.net`).

The command-line tool [s546126/termwise](https://github.com/s546126/termwise) (`tw`) is a separate project that Relingo borrowed its rules from; Relingo does not call it or need it. Recommendation: leave that repository named `termwise`; renaming it would move no Persona data.

The page is the `read` contribution (`plugin_page`), titled 英文阅读 (English reading, 英文リーディング). It opens with one line that says what the page is for and what each mode does; a paragraph whose translation fails says to open the page again later, with the reason in brackets. Labels and the manifest text follow the repository's plain-Chinese wording; config keys, stored data and the `text_lookup` answers are unchanged by that wording. The selected mode, the optional extra paragraph, remembered words and successful translations are stored in the extension data directory as `reading.json`.

## Modes

| Mode | What the reader shows |
| --- | --- |
| Translation (翻译模式) | The original block, then the translation. This is the mode used until a choice is stored. |
| Vocabulary (生词模式) | The original block, with candidate words underlined. A remembered word keeps a stronger underline. |

Switching modes redraws from the original article. A translation block is removed before vocabulary marks are added, and vocabulary marks are removed before a translation is added, including when the same HTML is rendered twice.

The in-app page uses the hub's locale for its own labels (zh-Hans, en, ja). The sample article stays English. zh-Hans and ja show the Chinese gloss; en shows the English gloss when ECDICT has one.

Persona views cannot draw an underline inside a sentence, so the page keeps the sentence as text and lists the same candidate words as rows. With a Persona that supports `text_lookup`, the app itself handles the rest: tapping any English word in the page (or anywhere else in the app) shows its meaning and can save it, and with **在 App 里给记下的词加下划线** on, saved words are underlined in place.

For development only, `relingo preview` serves the same reader in a browser with underlines and a popup; users never need it:

```sh
go -C extensions/relingo run . preview -addr 127.0.0.1:8731 -data /tmp/relingo-reading
```

It uses the same article, lexicon, selection and `reading.json` format as the extension. `-data` defaults to a fresh temporary directory.

## What is underlined

A word is a candidate when the embedded lexicon has its lemma and the word is not a function word (articles, pronouns, prepositions, conjunctions, auxiliaries), a short uppercase acronym, or an everyday developer term from `devwords.txt`. The lexicon is a subset of [ECDICT](https://github.com/skywind3000/ECDICT) (MIT, Copyright (c) 2025 Linwei): headwords whose frequency rank is outside the most common 5000 and at most 20000, plus Collins or Oxford headwords outside that band, and inflected forms (`0:` and `p/d/i/3/s/r/t`) that point at those headwords. `dict/build.py` rebuilds `dict/lexicon.json.gz` from a local `ecdict.csv` (`ECDICT_CSV`). Words with no entry, and text inside `script`, `style`, `textarea`, `input`, `select`, `button`, `nav`, `header`, `footer`, links, `code`, `pre`, or an editable element, are left alone.

Candidate filtering and the developer word list (`devwords.txt`) were adapted from [s546126/termwise](https://github.com/s546126/termwise) and are bundled in the program; see [NOTICE.md](../../extensions/relingo/NOTICE.md).

## Word lookup

The app calls the program over the extension protocol. Every call includes `contribution` (`relingo`).

| Method | What it answers |
| --- | --- |
| `text_lookup.state` | `{underline_saved, words}`. `words` is the saved lemmas, lowercase, at most 5000. `underline_saved` is the setting of that name. |
| `text_lookup.lookup` | `{found, word, lemma, phonetic, senses, saved}` for `{word, context, lang, ui_lang}`. Unknown words answer `found: false`, not an error. `ui_lang` starting with `en` uses the English gloss when ECDICT has one; otherwise the Chinese gloss. |
| `text_lookup.save` | Remembers `{word}` on the same list as the reading page and answers `{saved: true}` when it is now saved. |
| `text_lookup.remove` | Forgets `{word}` and answers `{saved: false}`. |

Lookup uses the same lemmatization, contractions and typographic apostrophes as the reading page. A function word, a short uppercase acronym or an everyday developer term is not offered as a candidate and is not saved through `text_lookup.save`, though if it is already saved it still answers `text_lookup.lookup` and can be removed (including by its inflected forms) through `text_lookup.remove`. A word with no lexicon entry is not found and is not saved.

`underline_saved` is a boolean setting, default `false`, shown in the settings as **在 App 里给记下的词加下划线**. The reading page does not draw this underline itself; the app does, from `text_lookup.state`.

## Translation

Sentence translation calls `https://api.mymemory.translated.net` (no key, declared in `permissions.network`). The target is Chinese, or Japanese when the hub locale is Japanese. A warning, an empty body, or a transport error is shown as a failure under the original sentence. The extension does not replace that failure with text of its own. Successful responses are cached in `reading.json` so the same sentence is not requested again. MyMemory's free quota can run out; the page then shows the failure until a later request succeeds.

## Tests

```sh
go -C extensions/relingo test ./...
```

The tests use a stand-in translator, so they do not call MyMemory. They cover candidate selection, skipped regions, repeated words, idempotent saves, mode switching without leftover marks, the extra paragraph, locales, the browser reader, and the extension protocol.
