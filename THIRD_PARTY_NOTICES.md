# Third-party notices

Axon Pulse is licensed under the GNU Affero General Public License v3.0
(see `LICENSE`). The following components are distributed with Pulse under
their own terms.

## Shared Axon runtime packages

Pulse uses four packages of the Go module
`github.com/Taurine-Technology/axon-contracts/gen/go` (`buildinfo`,
`logging`, `compress`, `diagnostics`). The public repository carries a partial
copy of that module in `third_party/axon-contracts`.

- Copyright 2026 Taurine Technology (Pty) Ltd.
- License: Apache License 2.0. The full text and attribution notices ship
  with the copy as `third_party/axon-contracts/LICENSE` and `NOTICE`.

## Fonts embedded in the desktop application

The desktop frontend ships Latin subsets of two typefaces as WOFF2 files.
Both are licensed under the SIL Open Font License, Version 1.1, reproduced
below.

- **Manrope** (`desktop/frontend/manrope-latin.woff2`).
  Copyright 2018 The Manrope Project Authors
  (<https://github.com/googlefonts/manrope>).
- **IBM Plex Mono** (`desktop/frontend/plexmono-400-latin.woff2`,
  `plexmono-500-latin.woff2`, `plexmono-600-latin.woff2`).
  Copyright © 2017 IBM Corp. with Reserved Font Name "Plex"
  (<https://github.com/IBM/plex>).

## Go module dependencies

Compiled Go dependencies are restricted to the permissive licenses listed in
the `LICENSE_ALLOWLIST` variable of the `Makefile` (Apache-2.0, BSD-2-Clause,
BSD-3-Clause, MIT, ISC, MPL-2.0). `make licenses` enforces the allowlist. To
produce a full report of every dependency, its license, and its notice text
for a given build, run:

```sh
go run github.com/google/go-licenses/v2@v2.0.1 report ./cmd/axon-pulse
```

## Public measurement endpoints

Speed and responsiveness tests use Cloudflare's documented public speed-test
endpoints (<https://speed.cloudflare.com>). No Cloudflare code is included;
see `docs/architecture.md` for the endpoint policy.

---

## SIL Open Font License, Version 1.1

```text
SIL OPEN FONT LICENSE Version 1.1 - 26 February 2007

PREAMBLE
The goals of the Open Font License (OFL) are to stimulate worldwide
development of collaborative font projects, to support the font creation
efforts of academic and linguistic communities, and to provide a free and
open framework in which fonts may be shared and improved in partnership
with others.

The OFL allows the licensed fonts to be used, studied, modified and
redistributed freely as long as they are not sold by themselves. The
fonts, including any derivative works, can be bundled, embedded,
redistributed and/or sold with any software provided that any reserved
names are not used by derivative works. The fonts and derivatives,
however, cannot be released under any other type of license. The
requirement for fonts to remain under this license does not apply
to any document created using the fonts or their derivatives.

DEFINITIONS
"Font Software" refers to the set of files released by the Copyright
Holder(s) under this license and clearly marked as such. This may
include source files, build scripts and documentation.

"Reserved Font Name" refers to any names specified as such after the
copyright statement(s).

"Original Version" refers to the collection of Font Software components as
distributed by the Copyright Holder(s).

"Modified Version" refers to any derivative made by adding to, deleting,
or substituting -- in part or in whole -- any of the components of the
Original Version, by changing formats or by porting the Font Software to a
new environment.

"Author" refers to any designer, engineer, programmer, technical
writer or other person who contributed to the Font Software.

PERMISSION & CONDITIONS
Permission is hereby granted, free of charge, to any person obtaining
a copy of the Font Software, to use, study, copy, merge, embed, modify,
redistribute, and sell modified and unmodified copies of the Font
Software, subject to the following conditions:

1) Neither the Font Software nor any of its individual components,
in Original or Modified Versions, may be sold by itself.

2) Original or Modified Versions of the Font Software may be bundled,
redistributed and/or sold with any software, provided that each copy
contains the above copyright notice and this license. These can be
included either as stand-alone text files, human-readable headers or
in the appropriate machine-readable metadata fields within text or
binary files as long as those fields can be easily viewed by the user.

3) No Modified Version of the Font Software may use the Reserved Font
Name(s) unless explicit written permission is granted by the corresponding
Copyright Holder. This restriction only applies to the primary font name as
presented to the users.

4) The name(s) of the Copyright Holder(s) or the Author(s) of the Font
Software shall not be used to promote, endorse or advertise any
Modified Version, except to acknowledge the contribution(s) of the
Copyright Holder(s) and the Author(s) or with their explicit written
permission.

5) The Font Software, modified or unmodified, in part or in whole,
must be distributed entirely under this license, and must not be
distributed under any other license. The requirement for fonts to
remain under this license does not apply to any document created
using the Font Software.

TERMINATION
This license becomes null and void if any of the above conditions are
not met.

DISCLAIMER
THE FONT SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO ANY WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT
OF COPYRIGHT, PATENT, TRADEMARK, OR OTHER RIGHT. IN NO EVENT SHALL THE
COPYRIGHT HOLDER BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY,
INCLUDING ANY GENERAL, SPECIAL, INDIRECT, INCIDENTAL, OR CONSEQUENTIAL
DAMAGES, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
FROM, OUT OF THE USE OR INABILITY TO USE THE FONT SOFTWARE OR FROM
OTHER DEALINGS IN THE FONT SOFTWARE.
```
