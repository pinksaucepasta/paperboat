# X-02 rename part by hand (own machines fx02f-l pbvm2 container, fx02f-m macpool-6, fx02f-w winpool-5; pb dev-918afefe9; server .11)

Run 1 (all six directions; a = machine with a [machine.<a>] section, b renames a; loop stops at the first non-healthy state, so L>W and M>W there are inconclusive):
```
L>M before rename: machine_section {"alias": "fx02f-l", "state": "healthy", "error_code": null, "recovery_actions": null}
  3 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  15 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  26 {"alias": "fx02f-l-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.fx02f-l] section, but no machine of yours is named \"fx02f-l\". Check the name with `pb machine list`."] source os
  apply 2 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT L>M paused_configuration_changed=True apply_restored=True
M>L before rename: machine_section {"alias": "fx02f-m", "state": "healthy", "error_code": null, "recovery_actions": null}
  3 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  15 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  27 {"alias": "fx02f-m-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.fx02f-m] section, but no machine of yours is named \"fx02f-m\". Check the name with `pb machine list`."] source os
  apply 5 {"alias": "fx02f-m-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 17 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT M>L paused_configuration_changed=True apply_restored=True
L>W before rename: machine_section {"alias": "fx02f-l", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
  3 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  15 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  27 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  38 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  50 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  60 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  72 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  82 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  93 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  103 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  115 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  126 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  136 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  147 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  157 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  168 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
 warnings ["The shared configuration has a [machine.fx02f-l] section, but no machine of yours is named \"fx02f-l\". Check the name with `pb machine list`."] source os
  apply 3 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT L>W paused_configuration_changed=False apply_restored=True
W>L before rename: machine_section {"alias": "fx02f-w", "state": "healthy", "error_code": null, "recovery_actions": null}
  3 {"alias": "fx02f-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  15 {"alias": "fx02f-w-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.fx02f-w] section, but no machine of yours is named \"fx02f-w\". Check the name with `pb machine list`."] source os
  apply 5 {"alias": "fx02f-w-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 17 {"alias": "fx02f-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT W>L paused_configuration_changed=True apply_restored=True
M>W before rename: machine_section {"alias": "fx02f-m", "state": "healthy", "error_code": null, "recovery_actions": null}
  3 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  15 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  27 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  38 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  50 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  62 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  74 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  85 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  98 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  110 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  122 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  133 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  145 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  157 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  170 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  181 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  193 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  205 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  217 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  229 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  241 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
 warnings ["The shared configuration has a [machine.fx02f-m] section, but no machine of yours is named \"fx02f-m\". Check the name with `pb machine list`."] source os
  apply 5 {"alias": "fx02f-m-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 17 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT M>W paused_configuration_changed=False apply_restored=True
W>M before rename: machine_section {"alias": "fx02f-w", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
  3 {"alias": "fx02f-w-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.fx02f-w] section, but no machine of yours is named \"fx02f-w\". Check the name with `pb machine list`."] source os
  apply 5 {"alias": "fx02f-w-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 17 {"alias": "fx02f-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT W>M paused_configuration_changed=True apply_restored=True
```

Run 2 (L>W, M>W again, healthy baseline, 480 s window):
```
L>W before rename: machine_section {"alias": "fx02f-l", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
  2 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  14 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  25 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  35 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  46 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  56 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  67 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  79 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  90 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  101 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  111 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  123 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  134 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  145 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  155 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  166 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  176 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  187 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  197 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  208 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  218 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  229 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  239 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  250 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  260 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  270 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  281 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  292 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  302 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  313 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  323 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  334 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  345 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  355 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  366 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  376 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  386 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  396 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  408 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  419 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  429 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  440 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  453 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  467 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  478 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
 warnings ["The shared configuration has a [machine.fx02f-l] section, but no machine of yours is named \"fx02f-l\". Check the name with `pb machine list`."] source os
  apply 2 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 18 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT L>W paused_configuration_changed=False apply_restored=True
M>W before rename: machine_section {"alias": "fx02f-m", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
  4 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  15 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  27 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  39 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  53 {"alias": "fx02f-m-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  65 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  77 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  88 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  100 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  113 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  125 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  137 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  149 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  160 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  173 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  185 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  197 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  209 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  223 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  235 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  246 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  258 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  270 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  282 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  294 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  306 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  318 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  329 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  341 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  353 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  365 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  377 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  389 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  401 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  413 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  424 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  436 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  448 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  460 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  472 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
 warnings ["The shared configuration has a [machine.fx02f-m] section, but no machine of yours is named \"fx02f-m\". Check the name with `pb machine list`."] source os
  apply 5 {"alias": "fx02f-m-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 17 {"alias": "fx02f-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT M>W paused_configuration_changed=False apply_restored=True
```

Run 3 (L>W, L>M, healthy baseline, 480 s window): no configuration_changed in either; the "pass" results in runs 1-2 above come from the shared-file edit pausing the machine, not from the rename. CONCLUSION: the rename part FAILS.
```
/tmp/claude-1001/-home-ubuntu-paperboat/e4362ca1-99c1-46cd-b9fe-f6c42796f0f5/scratchpad/rename2.py:45: SyntaxWarning: invalid escape sequence '\|'
  j = a.run(['sh', '-c', "journalctl --user -u paperboatd --since @%d --no-pager | grep -i 'config\|alias\|descriptor\|machine\|sync' | cut -c1-240 | tail -n 40" % t_rename]).text
L>W before rename: machine_section {"alias": "fx02f-l", "state": "healthy", "error_code": null, "recovery_actions": null}
 rename returned after 8 s
  44 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
 warnings ["The shared configuration has a [machine.fx02f-l] section, but no machine of yours is named \"fx02f-l\". Check the name with `pb machine list`."] source os
 JOURNAL 
  apply 2 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT L>W paused_configuration_changed=False apply_restored=True
L>M before rename: machine_section {"alias": "fx02f-l", "state": "healthy", "error_code": null, "recovery_actions": null}
 rename returned after 9 s
  291 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  411 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
 warnings ["The shared configuration has a [machine.fx02f-l] section, but no machine of yours is named \"fx02f-l\". Check the name with `pb machine list`."] source os
 JOURNAL 
  apply 1 {"alias": "fx02f-l-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 12 {"alias": "fx02f-l-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT L>M paused_configuration_changed=False apply_restored=True
```
