.version == 2
  and .provider == $provider
  and .model == $model
  and .source_revision == $source_revision
  and (.probe_event_ids | type == "object")
  and ((.probe_event_ids | keys | sort) == ($ghosts | sort))
  and (.probe_event_ids | to_entries | all(.value | type == "string" and length > 0))
