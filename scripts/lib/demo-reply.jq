def successful_body:
  type == "string" and length > 0
    and (
      if $provider == "demo" and $model == "fgentic-demo" then
        . == $expected_demo_reply
      else
        (startswith("⚠️") | not)
          and . != "⏳ working on it…"
          and . != "(the agent returned no content)"
          and (startswith("--- BEGIN FGENTIC BRIDGE PROVENANCE ---") | not)
      end
    );

[
  (.events_before[]?, .events_after[]?)
  | select(
      .type == "m.room.message"
        and .sender == $sender
        and .content.msgtype == "m.notice"
    )
] as $events
| $events[]
| select(
    .content."m.relates_to"."m.in_reply_to".event_id == $event_id
      and (.event_id | type == "string" and length > 0)
  )
| . as $reply
| (
    $reply.content.body,
    (
      $events[]
      | select(
          .content."m.relates_to".rel_type == "m.replace"
            and .content."m.relates_to".event_id == $reply.event_id
            and .content."m.new_content".msgtype == "m.notice"
        )
      | .content."m.new_content".body
    )
  )
| select(successful_body)
