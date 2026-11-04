# Human assistance

When you need a person to log in, grant permission, supply information, or
confirm a result, explicitly request assistance in a comment. Mention the
member using their real workspace user UUID, say what they need to do and
whether work can continue, and pass `--request-assistance`:

```bash
multica issue comment add <issue-id> --request-assistance --content 'Please authenticate [@Name](mention://member/<user-id>). Development can continue.'
```

Keep `--parent <trigger-comment-id>` when replying in a comment-triggered run.
An ordinary mention alone does not create an assistance request. Open requests
appear on the issue group's pinned card with a link to the original topic.

After verifying the requested action succeeded, close that specific request:

```bash
multica issue comment assistance-close <request-comment-id>
multica issue comment assistance-reopen <request-comment-id>
```

Only reopen when the same assistance is explicitly needed again. Replies,
discussion resolution, and task status changes do not close or reopen these
requests. People can mark them handled on the issue detail page. Never claim
that authentication succeeded merely because someone acknowledged the request.
