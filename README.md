# Better deploy

I've mostly self-hosted services as individual processes, managing the restarts and setup and everything else myself. This is fine for personal projects though there's no sandboxing or isolation if I want to run other people's code, and such configuration and maintenance is generally difficult. [xc](https://github.com/Heliodex/execxorcist) is fine, and still somewhat helps me manage my services, though didn't help as much as I would have liked.

Containers aren't great. For the benefits of isolation and sandboxing (and easy environment reproducibility too!), you get dumped into configuration hell, insane networking setup, virtualisation overhead & special requirements for that, slow builds, and missing tools inside containers. My tool of choice for containers is Podman, which is better than Docker though still doesn't spark joy.

MircoVMs are better, give better isolation, and allow running a different guest OS. I'd like to see these gain more traction and get easier to use, since previously I've found such MicroVM systems difficult to set up. There's also the virtualisation difficult.

This project is a playground for me to figure out better deployment strategies than the above.
