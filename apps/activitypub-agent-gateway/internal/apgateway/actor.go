package apgateway

import (
	"fmt"

	vocab "github.com/go-ap/activitypub"
)

// contentType is the canonical ActivityStreams 2.0 media type every AP object is served with.
const contentType = `application/activity+json; charset=utf-8`

// actorID returns the stable public IRI identifying a ghost's Service actor.
func (g *Gateway) actorID(ghost string) vocab.IRI {
	return vocab.IRI(fmt.Sprintf("%s/ap/agents/%s", g.baseURL, ghost))
}

func (g *Gateway) inboxID(ghost string) vocab.IRI  { return g.actorID(ghost) + "/inbox" }
func (g *Gateway) outboxID(ghost string) vocab.IRI { return g.actorID(ghost) + "/outbox" }

// buildActor renders a ghost as an ActivityPub Service actor. Service typing is the honest
// machine label at the actor level — a Fediverse client sees a bot, not a person. Richer
// per-reply bot/automated semantics land with the attribution work (docs/fediverse.md §3).
func (g *Gateway) buildActor(ghost string, ref AgentRef) *vocab.Service {
	id := g.actorID(ghost)
	actor := vocab.ServiceNew(id)
	actor.PreferredUsername = vocab.NaturalLanguageValuesNew(vocab.DefaultLangRef(ghost))
	actor.Name = vocab.NaturalLanguageValuesNew(vocab.DefaultLangRef(ghost))
	actor.Summary = vocab.NaturalLanguageValuesNew(vocab.DefaultLangRef(ref.Description))
	actor.Inbox = g.inboxID(ghost)
	actor.Outbox = g.outboxID(ghost)
	actor.URL = id
	return actor
}

// handle returns the fediverse handle (acct:agent-<name>@<serverName>) for a ghost.
func (g *Gateway) handle(ghost string) string {
	return fmt.Sprintf("acct:%s@%s", ghost, g.serverName)
}
