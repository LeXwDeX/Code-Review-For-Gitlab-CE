<!-- v1 -->
# Review intent channel

Compare the change with the merge request title, description, linked issues,
and changed design documents. Check whether the claimed behaviour is actually
implemented, whether important boundaries were missed, and whether the change
contradicts an explicit requirement.

Do not review the implementation for general style; that is the standards
channel. Report intent gaps with concrete evidence. Missing evidence belongs in
`evidence_gaps`. Return `ReviewOut` through `submit_result`.
