# Selling Eraser to US users — plan

**Status: proposal, nothing implemented.** Nothing here has been validated with customers, a
lawyer, or an accountant. Sections marked **VERIFY** are things I could not check from inside the
repo and that would be expensive to get wrong.

## 1. What we would actually be selling

Facts from the tree, because they constrain everything below:

- Eraser is a **local-first Go binary**: a CLI plus a web UI bound to `127.0.0.1`. There is no
  server, no account, no hosted component. It sends from **the user's own mailbox** over their own
  SMTP credentials and reads replies over their own IMAP.
- The broker database is a **YAML file shipped with the binary** (~850 entries), now partly
  self-healing from the user's own bounces.
- The licence is **MIT**, with two copyright holders (the original author and the fork maintainer).

That last point is the load-bearing one for a commercial plan.

### The licensing reality

MIT lets you sell this. It also lets *anyone else* take your paid version, strip the price, and
give it away — including the code you add. You must keep both copyright notices and the licence
text in anything you ship.

So the moat cannot be the code. It can be:

- **Freshness of the broker data** (a maintained, verified list is the actual product),
- **The service wrapped around it** (support, done-for-you setup, reporting),
- **Brand and distribution** (people buy the one they've heard of),
- **Convenience** (a signed, notarised installer beats `go build` for 99% of buyers).

If none of those appeal, the honest answer is that this is a great open-source project and a poor
product, and the plan should stop here.

## 2. The market

Paid competitors, approximate US consumer pricing — **VERIFY current numbers before quoting them
anywhere**:

| Service | Rough price | Model |
|---|---|---|
| DeleteMe | ~$130/yr | Done-for-you, human-assisted |
| Incogni | ~$80–100/yr | Automated, subscription |
| Optery | Free tier + ~$100–250/yr | Automated + screenshot proof |
| Kanary | ~$180/yr | Automated + monitoring |
| EasyOptOuts | ~$20/yr | Automated, deliberately cheap |

Every one of them requires the customer to **hand over their name, address history, phone number,
email, relatives, and often a photo ID** to a company whose business is holding exactly that data.
That is the wedge.

### Positioning

> **Your data never leaves your computer.** Everyone else fights data brokers by becoming one.

Eraser's architecture is the marketing claim, and it is unusually defensible because it is
*structurally* true rather than a promise: the app runs locally and sends from the user's own email
account. Brokers reply to *them*, not to a middleman. There is no vendor breach that can expose
them, because there is no vendor database.

Second claim, weaker but real: **the paper trail is yours.** Requests come from your address, so
the replies, the timestamps and the refusals are evidence you hold — which matters if you ever
escalate to a regulator.

Third: **you can read the code.** A small but loud audience cares, and they are the ones who write
the Reddit comments that sell this to everyone else.

## 3. What has to be true before charging money

Gaps I can see from the code, roughly in order of how badly they'd hurt:

1. **Distribution.** There is no installer. A paying US consumer will not run `go build`. Needs:
   signed macOS `.app` (Apple Developer ID + notarisation, ~$99/yr), signed Windows installer (code
   signing cert, ~$100–400/yr), and probably a one-click "open the app" experience rather than
   "start a server and browse to localhost".
2. **Email setup is the funnel killer.** Today it wants a Gmail app password, which requires 2FA and
   a trip through Google's settings. Every step there loses buyers. Options: OAuth device flow for
   Gmail/Outlook (real work, and Google's verification process for restricted scopes is its own
   project — **VERIFY**), or a "send via our relay" mode that breaks the local-only promise, or
   ship a very good guided wizard and accept the drop-off.
3. **Nothing runs when the app is closed.** Requests are one-shot, replies are only scanned when the
   user clicks. A paid product needs a scheduler — a background agent or a "check every day" mode —
   or the value evaporates after week one.
4. **No proof artifact.** Competitors sell screenshots and reports. Eraser has all the raw material
   in `history.db` but no exportable "here is what was sent, here is what came back" PDF. This is
   the single most sellable feature not yet built.
5. **US coverage tilt.** The fork was customised for a Latvian GDPR user; the default template is
   GDPR and `EU-NOTES.md` is prominent. For US buyers the defaults need to flip to CCPA/state law,
   and the people-search brokers (the ones that publish home addresses) need to be the headline
   coverage, verified.
6. **Data freshness has no process.** The self-healing bounce cleanup helps each install locally,
   but there is no mechanism to push a corrected list to everyone. If the product's value is a good
   list, this is the product.

## 4. Pricing and packaging

Three shapes, with the tradeoffs that matter:

**(A) One-time licence + optional data subscription.** $39–59 one-time for the app; $19/yr optional
for broker-list updates and new templates. Fits the local-first story (no server needed to enforce
anything), fits the "not another subscription" sentiment that this audience holds strongly, and
matches the MIT reality — you're selling convenience and updates, not access. Weak recurring
revenue.

**(B) Freemium OSS + paid hosted.** Keep the local app free; sell a hosted version for people who
won't run software. Contradicts the entire positioning — you'd become the thing you're criticising —
and puts you in scope for holding consumer PII, with all the compliance that implies. **Not
recommended.**

**(C) Free core + paid Pro features.** Scheduling, the report export, monitoring/re-checks, multiple
household profiles behind a licence key. Recurring, and the free tier is your marketing. Risk: the
free/paid line is exactly where a fork appears, and a fork is legal under MIT.

**Recommendation: (A) with a path to (C).** Ship a paid, signed, easy-to-install build with a
perpetual licence and 12 months of list updates. Renewal is optional and cheap. It is honest, it
suits the audience, and it needs no server to start.

Practical notes:
- Use a **merchant of record** (Paddle, Lemon Squeezy, or similar) rather than raw Stripe. They
  handle US sales tax nexus across ~45 states, which is otherwise a genuine and boring liability.
  **VERIFY** current terms and fees.
- Licence keys must **validate offline** (signed key, public key in the binary). Phoning home would
  contradict the whole pitch.
- Keep the source public. Charging for a build of MIT software is legitimate and normal; pretending
  it's proprietary is not, and this audience will notice.

## 5. Distribution and growth

Ranked by fit, not by size:

1. **The broker database is an SEO asset and nobody is using it.** ~850 companies × "how to opt out
   of X" is a long-tail content engine generated from data you already maintain. Each page: what
   the company does, what they hold, the opt-out URL, the email, and a download CTA. This is the
   highest-leverage channel available and it compounds. It is also, conveniently, genuinely useful
   to people who never buy anything.
2. **Reddit** (r/privacy, r/degoogle, r/selfhosted, r/personalfinance for the identity-theft angle).
   Participate honestly as the author; these communities punish marketing and reward tools that
   respect them. The local-first angle is tailor-made for r/selfhosted.
3. **Hacker News / Show HN** — one shot, for the architecture story ("a data-broker opt-out tool
   that never sees your data"), not the price.
4. **Privacy YouTubers and newsletters** (Techlore, The Hated One, Naomi Brockwell, Ask Leo-style
   generalists). Affiliate-friendly, and their audiences buy privacy tools.
5. **Product Hunt** — modest, but a launch day and a backlink.
6. **Identity-theft adjacency**: people search for this after a breach notification. Content aimed
   at "I just got a breach letter, what now" converts far better than "data broker" as a term,
   which most people don't know.

Sequence, roughly:

- **Weeks 1–4:** close the gaps in §3 that block a purchase (installer + wizard + report export).
  Keep selling nothing.
- **Weeks 5–6:** put up a site, the first 50 opt-out pages, and a free build with a waitlist.
- **Week 7:** paid build live; Show HN + Reddit + one YouTube partner in the same week.
- **Ongoing:** 5–10 opt-out pages a week; a monthly "what the brokers replied" post using aggregate,
  anonymous data from your own runs — nobody else can publish that, and it is inherently linkable.

## 6. Legal and compliance — for counsel, not for me

I am flagging these because they are cheap to check now and expensive to discover later. **None of
this is legal advice; all of it needs a US attorney.**

- **Advertising claims.** The FTC requires substantiation. "Removes you from 850 brokers" is not
  defensible; "sends legally-grounded deletion requests to 850 brokers and tracks the replies" is.
  Write the claims conservatively from the start — it is much harder to walk them back.
- **Subscriptions**, if you go that way: ROSCA and the FTC's negative-option rules, plus state
  auto-renewal statutes (California's is the strictest) govern disclosure, consent and
  cancellation. A one-time licence avoids most of this, which is another argument for (A).
- **Authorized agent rules.** Under the CCPA a business can require a consumer to verify a request
  made by an agent, and agents have registration obligations in California. Eraser as-is sends
  *from the consumer's own address*, which likely keeps you out of agent status entirely — worth
  confirming, because it is a meaningful structural advantage over competitors who do act as
  agents. **VERIFY.**
- **Refunds and consumer protection** for digital goods; a clear, plain-English refund policy.
- **Privacy policy and terms**, even for a local app: your website, your checkout, and your update
  check all process something.
- **Trademark**: "Eraser" is a common word and almost certainly conflicts in software. Check before
  you print it on anything. **VERIFY.**
- **Entity and tax**: where you are resident vs. where you sell. A merchant of record absorbs much
  of this; it does not absorb your own income tax position.
- **Attribution**: the upstream MIT notice must ship with every paid build, and the fork's README
  should stay honest about what it is.

## 7. What would prove this works

Milestones, in order, each of which should be allowed to kill the plan:

1. **50 people install the free build and finish setup.** If setup completion is under ~40%, the
   product problem is the wizard, not the marketing.
2. **10 of them run a second send.** Retention after week one is the whole question for a tool like
   this.
3. **20 pre-orders / waitlist converts at the real price.** If a Show HN + Reddit push cannot
   produce 20 buyers, the price or the positioning is wrong — not the channel.
4. **First 100 sales.** Only then is the scheduler/monitoring work (§3.3) worth building.

Kill criteria worth naming in advance: if setup completion stays low after two wizard rewrites, or
if support load per customer exceeds roughly an hour, this is a service business rather than a
software business — and that is a different plan with different economics.

## 8. Open questions for you

1. Do you want a **business** (support obligations, refunds, a company) or a **funded project**
   (donations, sponsorship, no promises)? The answer changes almost everything above.
2. Are you willing to run **any** server-side component, or is local-only an absolute?
3. US entity, or sell from the EU into the US via a merchant of record?
4. How much of your time per week is this allowed to take, sustainably, for a year?
5. Is the goal revenue, reach, or leverage on the brokers themselves? A free tool with 100,000 users
   changes broker behaviour more than a paid one with 1,000 — those are genuinely different goals
   and only you can pick.
