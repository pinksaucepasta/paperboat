# X-02 rename pause, fleet dev-08d3c4462 (lanex2-l2, lanex2-m, lanex2-w), 2026-11-16 IST

Script: a has [machine.<a>] in the shared file; b runs `pb machine rename` on a; a status must reach configuration_changed, `pb config sync apply` must restore healthy. Logs of the six ordered pairs:

## run-M_L
```
L lanex2-l2 linux ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
M lanex2-m darwin ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
M>L before rename: machine_section {"alias": "lanex2-m", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 rename returned after 6 s
  0 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  9 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  18 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  27 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  37 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  45 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  54 {"alias": "lanex2-m-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.lanex2-m] section, but no machine of yours is named \"lanex2-m\". Check the name with `pb machine list`."] source os
  apply 0 {"alias": "lanex2-m-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
  apply 9 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT M>L paused_configuration_changed=True apply_restored=True
rc=0
```

## run2-LM
```
L lanex2-l2 linux ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
M lanex2-m darwin ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
L>M before rename: machine_section {"alias": "lanex2-l2", "state": "healthy", "error_code": null, "recovery_actions": null}
 rename returned after 4 s
  0 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  9 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  18 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  26 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  35 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  44 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  52 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  61 {"alias": "lanex2-l2-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.lanex2-l2] section, but no machine of yours is named \"lanex2-l2\". Check the name with `pb machine list`."] source os
 JOURNAL 
  apply 0 {"alias": "lanex2-l2-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
  apply 9 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT L>M paused_configuration_changed=True apply_restored=True
rc=0
```

## run2-LW
```
L lanex2-l2 linux ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
W lanex2-w windows ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
L>W before rename: machine_section {"alias": "lanex2-l2", "state": "healthy", "error_code": null, "recovery_actions": null}
 rename returned after 8 s
  0 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  9 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  18 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  26 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  35 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  44 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  52 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  61 {"alias": "lanex2-l2-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.lanex2-l2] section, but no machine of yours is named \"lanex2-l2\". Check the name with `pb machine list`."] source os
 JOURNAL 
  apply 0 {"alias": "lanex2-l2-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
  apply 9 {"alias": "lanex2-l2-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT L>W paused_configuration_changed=True apply_restored=True
W>L before rename: machine_section {"alias": "lanex2-w", "state": "healthy", "error_code": null, "recovery_actions": null}
 rename returned after 5 s
  2 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  14 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  24 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  34 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  44 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  54 {"alias": "lanex2-w-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.lanex2-w] section, but no machine of yours is named \"lanex2-w\". Check the name with `pb machine list`."] source os
  apply 2 {"alias": "lanex2-w-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 12 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT W>L paused_configuration_changed=True apply_restored=True
rc=0
```

## run2-MW
```
M lanex2-m darwin ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
W lanex2-w windows ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
M>W before rename: machine_section {"alias": "lanex2-m", "state": "healthy", "error_code": null, "recovery_actions": null}
 rename returned after 9 s
  0 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  9 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  18 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  27 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  36 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  44 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  53 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  62 {"alias": "lanex2-m-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.lanex2-m] section, but no machine of yours is named \"lanex2-m\". Check the name with `pb machine list`."] source os
  apply 0 {"alias": "lanex2-m-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
  apply 9 {"alias": "lanex2-m-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT M>W paused_configuration_changed=True apply_restored=True
rc=0
```

## run2-WM
```
M lanex2-m darwin ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
W lanex2-w windows ▄█▄       Paperboat
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-08d3c4462
W>M before rename: machine_section {"alias": "lanex2-w", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 rename returned after 6 s
  2 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  12 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  22 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  32 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  42 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
  52 {"alias": "lanex2-w-r", "state": "pending", "error_code": "configuration_changed", "recovery_actions": ["apply_configuration"]}
 warnings ["The shared configuration has a [machine.lanex2-w] section, but no machine of yours is named \"lanex2-w\". Check the name with `pb machine list`."] source os
  apply 2 {"alias": "lanex2-w-r", "state": "syncing", "error_code": null, "recovery_actions": null}
  apply 12 {"alias": "lanex2-w-r", "state": "healthy", "error_code": null, "recovery_actions": null}
RESULT W>M paused_configuration_changed=True apply_restored=True
rc=0
```
