gvm firewall

gvm does not manage the firewall in this build.

It opens no ports, writes no ufw or nftables rules, and changes nothing about
how your box is reachable. Opening the ports your servers listen on — and no
others — is yours to do with your own firewall.

`gvm ports` shows which ports your instances use and flags conflicts between
them, so you know what to open. It only reports; it changes nothing.

A later release will detect your firewall and offer to manage it. Until then,
any gvm output that mentions the firewall is telling you what to do yourself,
never something gvm did.
