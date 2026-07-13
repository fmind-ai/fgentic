(.events_before[]?, .events_after[]?)
| select(
  .type == "m.room.message"
    and .sender == $sender
    and .content.msgtype == "m.notice"
    and .content."m.relates_to"."m.in_reply_to".event_id == $event_id
    and (.content.body | type == "string" and length > 0)
    and (
      if $provider == "demo" and $model == "fgentic-demo" then
        .content.body == $expected_demo_reply
      else
        (.content.body | startswith("⚠️") | not)
          and .content.body != "⏳ working on it…"
          and .content.body != "(the agent returned no content)"
      end
    )
)
