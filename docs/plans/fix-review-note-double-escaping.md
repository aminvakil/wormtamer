# Fix review note HTML and Markdown double escaping

Status: proposed

## Goal

Render model-controlled review text as readable plain text in GitLab without exposing active HTML or Markdown. In particular, ordinary apostrophes and quotation marks must not appear as literal escaped entities such as `&\#39;` or `&\#34;` in published notes.

## Scope

- Correct text escaping for the review summary and every model-controlled finding field rendered by `internal/review`.
- Preserve neutralization of active HTML, Markdown formatting, links, mentions, quick actions, and other GitLab-interpreted constructs.
- Preserve configured-secret rejection, finding identifier validation, the application-owned publication marker, and the rendered-note size limit.
- Add focused regression coverage for the observed apostrophe and quotation-mark output and for the existing security properties.

Do not allow model-supplied Markdown or HTML, change the review result schema, alter note layout beyond escaping required for readable text, update already-published notes, or change GitLab publication and reconciliation behavior.

## Approach

Replace the blanket `html.EscapeString` step in the plain-text renderer with escaping appropriate for an HTML text node before Markdown interpretation. Escape the characters needed to prevent HTML constructs, including ampersands and angle brackets, without entity-encoding ordinary apostrophes or quotation marks that are safe in text content. Continue escaping Markdown metacharacters and inserting the existing zero-width separator after `@` so model text cannot create formatting, links, mentions, or commands.

Keep the transformation centralized in the existing renderer so summaries, titles, severities, paths, explanations, and recommendations use one policy. Do not infer or preserve formatting from model output. Keep application-owned Markdown structure, finding identifiers, and the hidden publication marker outside model-controlled formatting decisions.

Extend the renderer tests with literal apostrophes and quotation marks, ampersands, angle brackets, strings resembling HTML entities, Markdown links and formatting, mentions, multiline quick-action-like text, and all rendered finding fields. Assert both readability of safe punctuation and inert representation of unsafe constructs.

## Risks and Open Questions

GitLab applies its own Markdown parser and HTML sanitizer after receiving the note. The emitted source must therefore remain safe under GitLab rendering rather than relying on browser HTML escaping alone. Focused fixtures should cover the syntax classes Wormtamer neutralizes; do not add a Markdown parser dependency or live GitLab test solely for this fix.

## Verification

- A summary containing `variable 'windows_public_ip' to 'windows_public_ips'` is published with readable apostrophes and never contains `&\#39;` or an equivalent visibly double-escaped form.
- Ordinary double quotes remain readable in summaries and finding details.
- Model-controlled `<script>`-like text is displayed as inert text and cannot create an HTML element.
- Model-controlled Markdown links, emphasis, headings, blockquotes, code spans, and list syntax remain inert rather than becoming formatting or links.
- Model-controlled mentions and quick-action-like multiline text cannot notify users or invoke GitLab commands.
- Literal ampersands and text resembling entities cannot become active markup or be double escaped into the observed broken form.
- Every model-controlled review field follows the same escaping behavior.
- Configured-secret rejection, finding identifier validation, publication markers, and rendered-note limits retain their current behavior.
