select(
  .logger == "pgaudit" and
  .msg == "record" and
  (.record.audit.class == "DDL" or .record.audit.class == "ROLE")
)
| {
  time: .record.log_time,
  pod: .logging_pod,
  database_role: .record.user_name,
  database: .record.database_name,
  session_id: .record.session_id,
  audit: {
    type: .record.audit.audit_type,
    statement_id: .record.audit.statement_id,
    substatement_id: .record.audit.substatement_id,
    class: .record.audit.class,
    command: .record.audit.command,
    object_type: (.record.audit.object_type // ""),
    object_name: (.record.audit.object_name // "")
  }
}
