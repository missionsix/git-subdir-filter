
# Go filter-branch

This tool is intended run periodically to sync a subdirectory from one git
repository to another, maintaining synchronization.

## Requirements

 * Source git repository branch with a subdirectory
 * Read-only protected target repository

The target branch must not be an active development branch.  The idea is
to have a tracking branch only watching a subdirectory.

This is useful if your master respository is large - but contains the master
source for a sub project.  Development may be taking place in both the master
and the external branches.

## Usage



